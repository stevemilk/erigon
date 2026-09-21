// Copyright 2026 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package era_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/e2store"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/era/eratest"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
)

// TestSyntheticEra1 exercises the reader against a file built here rather than a published one: the
// index arithmetic, the record skipping, the accumulator, and Verify.
func TestSyntheticEra1(t *testing.T) {
	const first, count = era.MaxBlocksPerFile, 64
	chain := eratest.NewChain(t, 0)
	chain.WriteFile(t, t.TempDir(), first, 1) // the epoch before, so the file under test has a parent
	path := chain.WriteFile(t, t.TempDir(), count, 2)

	r, err := era.OpenEra1(path)
	require.NoError(t, err)
	defer r.Close()

	require.Equal(t, uint64(first), r.Start())
	require.Equal(t, count, r.Count())
	require.Equal(t, uint64(first+count-1), r.Last())

	// Read out of order: the index has to locate a block without scanning.
	for _, num := range []uint64{first + count - 1, first, first + 7} {
		headerRLP, err := r.HeaderRLP(num)
		require.NoError(t, err)
		var header types.Header
		require.NoError(t, rlp.DecodeBytes(headerRLP, &header))
		require.Equal(t, num, header.Number.Uint64())

		bodyRLP, err := r.BodyRLP(num)
		require.NoError(t, err)
		var body types.RawBody
		require.NoError(t, rlp.DecodeBytes(bodyRLP, &body))
		require.Len(t, body.Transactions, 2)

		td, err := r.TotalDifficulty(num)
		require.NoError(t, err)
		require.Len(t, td, 32)
	}

	_, err = r.HeaderRLP(first - 1)
	require.Error(t, err, "a block before the file starts is out of range")
	_, err = r.HeaderRLP(first + count)
	require.Error(t, err, "a block after the file ends is out of range")
	// The file does not hold block 0, so a parent is what anchors it.
	_, err = era.Verify(r, chain.Parent(first), common.Hash{})
	require.NoError(t, err)
}

// TestVerifyChainsBlocks pins the difference between a file that is merely self-consistent and one
// that is part of a chain: two files built independently each verify on their own, but the second
// does not follow the first.
func TestVerifyChainsBlocks(t *testing.T) {
	dir := t.TempDir()
	chainA := eratest.NewChain(t, 0)
	firstPath := chainA.WriteFile(t, dir, 8, 1)
	chainB := eratest.NewChain(t, 0)
	chainB.WriteFile(t, t.TempDir(), era.MaxBlocksPerFile, 1)
	secondPath := chainB.WriteFile(t, t.TempDir(), 8, 1)

	first, err := era.OpenEra1(firstPath)
	require.NoError(t, err)
	defer first.Close()
	second, err := era.OpenEra1(secondPath)
	require.NoError(t, err)
	defer second.Close()

	last, err := era.Verify(first, common.Hash{}, chainA.Genesis())
	require.NoError(t, err)
	_, err = era.Verify(second, chainB.Parent(era.MaxBlocksPerFile), common.Hash{})
	require.NoError(t, err, "on its own the second file is self-consistent")

	_, err = era.Verify(second, last, common.Hash{})
	require.ErrorContains(t, err, "follows", "it should not continue the first file's chain")
}

// TestOpenRejectsCorruptFiles covers the guards on lengths and counts read off disk. Archives come
// from other people's exporters, so a damaged or crafted one has to be an error, never a panic.
func TestOpenRejectsCorruptFiles(t *testing.T) {
	corruptChain := eratest.NewChain(t, 0)
	good, err := os.ReadFile(corruptChain.WriteFile(t, t.TempDir(), 4, 1))
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		corrupt func([]byte) []byte
	}{
		{"truncated", func(b []byte) []byte { return b[:len(b)-16] }},
		{"empty index", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[len(b)-8:], 0)
			return b
		}},
		{"more blocks than an epoch holds", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[len(b)-8:], era.MaxBlocksPerFile+1)
			return b
		}},
		{"record longer than the file", func(b []byte) []byte {
			// Blow up the first record's length, which sits right after the version record.
			copy(b[e2store.HeaderLen+2:], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.corrupt(bytes.Clone(good))
			path := filepath.Join(t.TempDir(), "mainnet-00000-00000000.era1")
			require.NoError(t, os.WriteFile(path, raw, 0o644))

			r, err := era.OpenEra1(path)
			if err != nil {
				return // refused at open, which is the point
			}
			defer r.Close()
			_, err = era.Verify(r, common.Hash{}, corruptChain.Genesis())
			require.Error(t, err, "a corrupt file must not verify")
		})
	}
}

// TestVerifyTypedTransactions covers the encoding a legacy-only range never reaches: a typed
// transaction is stored in the body wrapped in an RLP string, while the transactions root is taken
// over the unwrapped bytes. Hashing them as stored passes on pre-Berlin blocks and fails on
// everything after.
func TestVerifyTypedTransactions(t *testing.T) {
	typedChain := eratest.NewChain(t, 0).UseTypedTxns(1)
	path := typedChain.WriteFile(t, t.TempDir(), 8, 3)

	r, err := era.OpenEra1(path)
	require.NoError(t, err)
	defer r.Close()

	bodyRLP, err := r.BodyRLP(r.Start())
	require.NoError(t, err)
	var body types.RawBody
	require.NoError(t, rlp.DecodeBytes(bodyRLP, &body))
	require.NotEmpty(t, body.Transactions)
	require.True(t, types.TypedTransactionMarshalledAsRlpString(body.Transactions[0]),
		"the fixture has to produce the wrapped form, or this proves nothing")

	_, err = era.Verify(r, common.Hash{}, typedChain.Genesis())
	require.NoError(t, err)
}

// TestVerifyCatchesSwappedBody swaps two bodies so every record stays well formed but two sit under
// the wrong headers. Snappy's checksums pass and so does the accumulator, which covers headers and
// total difficulties only; checking each body against the roots in its own header is what catches
// it, and that is the case that matters for an archive someone else produced.
func TestVerifyCatchesSwappedBody(t *testing.T) {
	chain := eratest.NewChain(t, 0)
	path := chain.WriteFile(t, t.TempDir(), 4, 2)

	r, err := era.OpenEra1(path)
	require.NoError(t, err)
	defer r.Close()

	blocks := make([]era.Block, 0, r.Count())
	for n := r.Start(); n <= r.Last(); n++ {
		b, err := r.Block(n)
		require.NoError(t, err)
		blocks = append(blocks, b)
	}
	require.NotEqual(t, blocks[0].BodyRLP, blocks[1].BodyRLP, "the swap has to change something")
	blocks[0].BodyRLP, blocks[1].BodyRLP = blocks[1].BodyRLP, blocks[0].BodyRLP

	var out bytes.Buffer
	w, err := era.NewEra1Writer(&out, r.Start())
	require.NoError(t, err)
	for _, b := range blocks {
		require.NoError(t, w.Add(b))
	}
	require.NoError(t, w.Finalize())

	tampered := filepath.Join(t.TempDir(), filepath.Base(path))
	require.NoError(t, os.WriteFile(tampered, out.Bytes(), 0o644))

	bad, err := era.OpenEra1(tampered)
	require.NoError(t, err, "the tampered file still parses, which is the point")
	defer bad.Close()
	_, err = era.Verify(bad, common.Hash{}, chain.Genesis())
	require.ErrorContains(t, err, "invalid transaction hash")
}
