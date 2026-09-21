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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/seg"
	"github.com/erigontech/erigon/db/snaptype"
	_ "github.com/erigontech/erigon/db/snaptype2" // registers the block segment types ParseFileName needs
	"github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
)

// segReader walks a .seg file word by word from its first word.
type segReader struct {
	d *seg.Decompressor
	g *seg.Getter
}

// openSeg opens the segment of the given type holding block num, and reports the block it starts at.
func openSeg(t *testing.T, dir, typ string, num uint64) (*segReader, uint64) {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "v*-"+typ+".seg"))
	require.NoError(t, err)
	for _, path := range m {
		info, _, ok := snaptype.ParseFileName(dir, filepath.Base(path))
		if !ok || num < info.From || num >= info.To {
			continue
		}
		d, err := seg.NewDecompressor(path)
		require.NoError(t, err)
		t.Cleanup(d.Close)
		return &segReader{d: d, g: d.MakeGetter()}, info.From
	}
	t.Fatalf("no %s segment in %s holds block %d", typ, dir, num)
	return nil, 0
}

func (s *segReader) next(t *testing.T) []byte {
	t.Helper()
	require.True(t, s.g.HasNext(), "segment ran out of words")
	w, _ := s.g.Next(nil)
	return w
}

func (s *segReader) skip(t *testing.T, n uint32) {
	t.Helper()
	for ; n > 0; n-- {
		require.True(t, s.g.HasNext(), "segment ran out of words")
		s.g.Skip()
	}
}

// TestEra1MatchesSegments decodes a real .era1 file and checks every block against the erigon
// segments covering the same range, in all three encodings, plus the accumulator root. Set
// ERA1_FILE to the .era1 file and ERA1_SNAPDIR to a snapshots dir holding the 000000-*
// headers/bodies/transactions segments. The file's range has to hold transactions for the
// transaction segment to be checked at all, so the very first mainnet files will not do.
func TestEra1MatchesSegments(t *testing.T) {
	file, snapDir := os.Getenv("ERA1_FILE"), os.Getenv("ERA1_SNAPDIR")
	if file == "" || snapDir == "" {
		t.Skip("set ERA1_FILE and ERA1_SNAPDIR to run")
	}

	r, err := era.Open(file)
	require.NoError(t, err)
	defer r.Close()

	headers, segFrom := openSeg(t, snapDir, "headers", r.Start())
	bodies, _ := openSeg(t, snapDir, "bodies", r.Start())
	txs, _ := openSeg(t, snapDir, "transactions", r.Start())

	// Walk the segments forward to the era1 file's first block. Only the bodies have to be read on
	// the way, to keep the transaction segment in step.
	var baseTxnID types.BaseTxnID
	headers.skip(t, uint32(r.Start()-segFrom))
	for n := segFrom; n < r.Start(); n++ {
		var stored types.BodyForStorage
		require.NoError(t, rlp.DecodeBytes(bodies.next(t), &stored))
		txs.skip(t, stored.TxCount)
		baseTxnID = types.BaseTxnID(stored.BaseTxnID.LastSystemTx(stored.TxCount) + 1)
	}

	// Sender recovery is fork-sensitive: a single latest signer rejects Frontier-era signatures, so
	// the signer has to be picked per block from the chain config.
	spec, err := chainspec.ChainSpecByName(os.Getenv("ERA1_CHAIN"))
	require.NoError(t, err, "set ERA1_CHAIN to the chain the era1 file belongs to")
	chainConfig := spec.Config

	var txnCount int
	var acc era.Accumulator
	for n := r.Start(); n <= r.Last(); n++ {
		hdrRLP, err := r.HeaderRLP(n)
		require.NoError(t, err)
		bodyRLP, err := r.BodyRLP(n)
		require.NoError(t, err)
		td, err := r.TotalDifficulty(n)
		require.NoError(t, err)

		// Header segment word: first byte of the block hash, then the header RLP verbatim.
		hw := headers.next(t)
		require.Equalf(t, hdrRLP, hw[1:], "header rlp differs at block %d", n)
		var h types.Header
		require.NoError(t, rlp.DecodeBytes(hdrRLP, &h))
		require.Equalf(t, h.Hash()[0], hw[0], "header hash prefix differs at block %d", n)
		require.NoError(t, acc.Add(h.Hash(), td))

		// Body segment word: BodyForStorage, which keeps the uncles and withdrawals but replaces the
		// transactions with a count, since they live in their own segment.
		var body types.RawBody
		require.NoError(t, rlp.DecodeBytes(bodyRLP, &body))
		var stored types.BodyForStorage
		require.NoError(t, rlp.DecodeBytes(bodies.next(t), &stored))
		require.Equalf(t, uint32(len(body.Transactions))+2, stored.TxCount, "tx count differs at block %d", n)
		require.Equalf(t, types.CalcUncleHash(body.Uncles), types.CalcUncleHash(stored.Uncles), "uncles differ at block %d", n)
		require.Equalf(t, baseTxnID, stored.BaseTxnID, "base txn id differs at block %d", n)
		baseTxnID = types.BaseTxnID(stored.BaseTxnID.LastSystemTx(stored.TxCount) + 1)

		// Transaction segment words: an empty word for each of the two system transactions bracketing
		// the block, and in between the first byte of the tx hash, the sender, then the envelope.
		require.Emptyf(t, txs.next(t), "expected the opening system txn at block %d", n)
		for i, want := range body.Transactions {
			tw := txs.next(t)
			require.Equalf(t, want, tw[21:], "txn %d rlp differs at block %d", i, n)
			txn, err := types.DecodeTransaction(want)
			require.NoError(t, err)
			require.Equalf(t, txn.Hash()[0], tw[0], "txn %d hash prefix differs at block %d", i, n)
			// The sender is not in the era1 file, so a converter has to recover it. Check that what
			// recovery produces is what the segment stores.
			signer := types.MakeSigner(chainConfig, n, h.Time)
			sender, err := txn.Sender(*signer)
			require.NoErrorf(t, err, "txn %d sender recovery failed at block %d", i, n)
			addr := sender.Value()
			require.Equalf(t, tw[1:21], addr[:], "txn %d sender differs at block %d", i, n)
			txnCount++
		}
		require.Emptyf(t, txs.next(t), "expected the closing system txn at block %d", n)
	}
	stored, err := r.Accumulator()
	require.NoError(t, err)
	root, err := acc.Root()
	require.NoError(t, err)
	require.Equal(t, stored, root, "accumulator root differs from the one stored in the file")

	require.NotZero(t, txnCount, "this file's range holds no transactions, so nothing checked the transaction segment: pick a later era1 file")
	t.Logf("compared blocks %d..%d (%d transactions) against the segments in all three encodings", r.Start(), r.Last(), txnCount)
}
