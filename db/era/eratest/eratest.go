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

// Package eratest builds era1 files for tests that should not depend on a published archive.
package eratest

import (
	"bytes"
	"crypto/ecdsa"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/empty"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
)

// Chain builds era1 files that continue one another, since a run is verified as a single chain.
// Headers are minimal but their roots are real, so the files pass era.Verify.
type Chain struct {
	key     *ecdsa.PrivateKey
	signer  *types.Signer
	parent  common.Hash
	genesis common.Hash
	td      uint256.Int
	next    uint64
	nonce   uint64
	typed   bool
	hashes  map[uint64]common.Hash
}

// NewChain starts a chain at the given block.
func NewChain(t testing.TB, first uint64) *Chain {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	// Frontier rules, so the blocks can sit at the start of a chain whose early forks reject
	// replay-protected signatures.
	return &Chain{key: key, signer: types.MakeFrontierSigner(), next: first, hashes: map[uint64]common.Hash{}}
}

// UseTypedTxns makes the blocks carry EIP-1559 transactions, which sit in a body wrapped in an RLP string
// while the transactions root is taken over the unwrapped bytes. Code that hashes them as stored
// gets the root wrong, and only a typed transaction shows it.
func (c *Chain) UseTypedTxns(chainID uint64) *Chain {
	c.typed = true
	c.signer = types.LatestSignerForChainID(uint256.NewInt(chainID))
	return c
}

// WriteFile appends one file of n blocks to the chain and writes it into dir, named the way the
// formats are. Returns the path.
func (c *Chain) WriteFile(t testing.TB, dir string, n, txnsPerBlock int) string {
	t.Helper()
	first := c.next

	var out bytes.Buffer
	w, err := era.NewEra1Writer(&out, first)
	require.NoError(t, err)
	for range n {
		require.NoError(t, w.Add(c.block(t, txnsPerBlock)))
	}
	require.NoError(t, w.Finalize())

	path := filepath.Join(dir, fmt.Sprintf("mainnet-%05d-00000000.era1", first/era.MaxBlocksPerFile))
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o644))
	return path
}

func (c *Chain) block(t testing.TB, txnsPerBlock int) era.Block {
	t.Helper()
	txns := make([]types.Transaction, txnsPerBlock)
	raw := make([][]byte, txnsPerBlock)
	for i := range txns {
		if c.typed {
			txns[i] = types.MustSignNewTx(c.key, *c.signer, &types.DynamicFeeTransaction{
				CommonTx: types.CommonTx{Nonce: c.nonce, GasLimit: 21000},
				ChainID:  *uint256.NewInt(1),
				TipCap:   *uint256.NewInt(1),
				FeeCap:   *uint256.NewInt(1),
			})
		} else {
			txns[i] = types.MustSignNewTx(c.key, *c.signer, &types.LegacyTx{
				CommonTx: types.CommonTx{Nonce: c.nonce, GasLimit: 21000},
				GasPrice: *uint256.NewInt(1),
			})
		}
		c.nonce++
		var err error
		raw[i], err = rlp.EncodeToBytes(txns[i])
		require.NoError(t, err)
	}
	bodyRLP, err := rlp.EncodeToBytes(&types.RawBody{Transactions: raw})
	require.NoError(t, err)

	header := &types.Header{
		ParentHash: c.parent,
		UncleHash:  empty.UncleHash,
		Root:       empty.RootHash,
		TxHash:     types.DeriveSha(types.Transactions(txns)),
		Difficulty: *uint256.NewInt(1),
		Number:     *uint256.NewInt(c.next),
		GasLimit:   30_000_000,
		Time:       c.next,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	require.NoError(t, err)

	c.parent = crypto.Keccak256Hash(headerRLP)
	c.hashes[c.next] = c.parent
	if c.genesis == (common.Hash{}) {
		c.genesis = c.parent
	}
	c.td.AddUint64(&c.td, 1)
	block := era.Block{Number: c.next, HeaderRLP: headerRLP, BodyRLP: bodyRLP, TotalDiff: leBytes32(&c.td)}
	c.next++
	return block
}

// leBytes32 encodes v the way the format stores total difficulty.
func leBytes32(v *uint256.Int) []byte {
	be := v.Bytes32()
	out := make([]byte, 32)
	for i := range be {
		out[i] = be[31-i]
	}
	return out
}

// Genesis is the hash of the chain's first block, which import anchors on.
func (c *Chain) Genesis() common.Hash { return c.genesis }

// Parent is the hash of the block before num, which anchors a file that does not hold block 0.
func (c *Chain) Parent(num uint64) common.Hash { return c.hashes[num-1] }
