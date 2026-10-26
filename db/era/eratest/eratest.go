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
	"slices"
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
	chainID *uint256.Int
	roots   bool
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
	c.chainID = uint256.NewInt(chainID)
	c.signer = types.LatestSignerForChainID(c.chainID)
	return c
}

// UsePostStateRoots makes the receipts carry a post-state root, as before Byzantium, instead of a status.
func (c *Chain) UsePostStateRoots() *Chain {
	c.roots = true
	return c
}

// WriteFile appends one file of n blocks to the chain and writes it into dir, named the way the
// formats are. Returns the path.
func (c *Chain) WriteFile(t testing.TB, dir string, n, txnsPerBlock int) string {
	t.Helper()
	first := c.next

	blocks := make([]era.Block, n)
	for i := range blocks {
		blocks[i] = c.block(t, txnsPerBlock)
	}
	path := filepath.Join(dir, fmt.Sprintf("mainnet-%05d-00000000.era1", first/era.MaxBlocksPerFile))
	write(t, path, first, blocks)
	return path
}

// Rewrite reads the file at path, lets edit change its blocks, and writes it back.
func Rewrite(t testing.TB, path string, edit func(blocks []era.Block)) {
	t.Helper()
	r, err := era.OpenEra1(path)
	require.NoError(t, err)
	blocks := make([]era.Block, r.Count())
	for i := range blocks {
		num := r.Start() + uint64(i)
		b := &blocks[i]
		b.HeaderRLP, err = r.HeaderRLP(num)
		require.NoError(t, err)
		b.BodyRLP, err = r.BodyRLP(num)
		require.NoError(t, err)
		b.ReceiptsRLP, err = r.ReceiptsRLP(num)
		require.NoError(t, err)
		b.TotalDiff, err = r.TotalDifficulty(num)
		require.NoError(t, err)
	}
	first := r.Start()
	require.NoError(t, r.Close())
	edit(blocks)
	write(t, path, first, blocks)
}

func write(t testing.TB, path string, first uint64, blocks []era.Block) {
	t.Helper()
	var out bytes.Buffer
	w, err := era.NewWriter(&out, first)
	require.NoError(t, err)
	for _, b := range blocks {
		require.NoError(t, w.Add(b))
	}
	require.NoError(t, w.Finalize())
	require.NoError(t, os.WriteFile(path, out.Bytes(), 0o644))
}

func (c *Chain) block(t testing.TB, txnsPerBlock int) era.Block {
	t.Helper()
	if c.next == 0 {
		txnsPerBlock = 0 // a genesis block carries no transactions
	}
	txns := make([]types.Transaction, txnsPerBlock)
	raw := make([][]byte, txnsPerBlock)
	receipts := make(types.Receipts, txnsPerBlock)
	for i := range txns {
		// Even transactions create a contract, odd ones call one, so receipts carry both kinds.
		var to *common.Address
		if i%2 == 1 {
			to = &common.Address{0xc0, byte(i)}
		}
		if c.typed {
			txns[i] = types.MustSignNewTx(c.key, *c.signer, &types.DynamicFeeTransaction{
				CommonTx: types.CommonTx{Nonce: c.nonce, GasLimit: 21000, To: to},
				ChainID:  *c.chainID,
				TipCap:   *uint256.NewInt(1),
				FeeCap:   *uint256.NewInt(1),
			})
		} else {
			txns[i] = types.MustSignNewTx(c.key, *c.signer, &types.LegacyTx{
				CommonTx: types.CommonTx{Nonce: c.nonce, GasLimit: 21000, To: to},
				GasPrice: *uint256.NewInt(1),
			})
		}
		c.nonce++
		var err error
		raw[i], err = rlp.EncodeToBytes(txns[i])
		require.NoError(t, err)
		receipts[i] = c.receipt(txns[i], i)
	}
	bodyRLP, err := rlp.EncodeToBytes(&types.RawBody{Transactions: raw})
	require.NoError(t, err)
	receiptsRLP, err := rlp.EncodeToBytes(receipts)
	require.NoError(t, err)

	header := &types.Header{
		ParentHash:  c.parent,
		UncleHash:   empty.UncleHash,
		Root:        empty.RootHash,
		TxHash:      types.DeriveSha(types.Transactions(txns)),
		ReceiptHash: types.DeriveSha(receipts),
		Bloom:       types.CreateBloom(receipts),
		Difficulty:  *uint256.NewInt(1),
		Number:      *uint256.NewInt(c.next),
		GasLimit:    30_000_000,
		GasUsed:     uint64(txnsPerBlock) * 21000,
		Time:        c.next,
	}
	headerRLP, err := rlp.EncodeToBytes(header)
	require.NoError(t, err)

	c.parent = crypto.Keccak256Hash(headerRLP)
	c.hashes[c.next] = c.parent
	if c.genesis == (common.Hash{}) {
		c.genesis = c.parent
	}
	c.td.AddUint64(&c.td, 1)
	block := era.Block{HeaderRLP: headerRLP, BodyRLP: bodyRLP, ReceiptsRLP: receiptsRLP, TotalDiff: leBytes32(&c.td)}
	c.next++
	return block
}

// receipt is the consensus receipt of the i-th transaction of the block being built: every
// transaction succeeds, uses 21000 gas and emits one log, so log indexes and cumulative gas grow
// with the position in the block.
func (c *Chain) receipt(txn types.Transaction, i int) *types.Receipt {
	r := &types.Receipt{
		Type:              txn.Type(),
		Status:            types.ReceiptStatusSuccessful,
		CumulativeGasUsed: uint64(i+1) * 21000,
		Logs: []*types.Log{{
			Address: LogAddress(c.next, i),
			Topics:  []common.Hash{LogTopic(c.next, i)},
			Data:    []byte{byte(i)},
		}},
	}
	if c.roots {
		root := PostStateRoot(c.next, i)
		r.PostState = root[:]
	}
	return r
}

// PostStateRoot is the root the i-th receipt of block num carries under UsePostStateRoots.
func PostStateRoot(num uint64, i int) common.Hash {
	return common.Hash{0xcc, byte(num >> 8), byte(num), byte(i)}
}

// LogAddress and LogTopic are what the log of the i-th transaction of block num carries.
func LogAddress(num uint64, i int) common.Address {
	return common.Address{0xaa, byte(num >> 8), byte(num), byte(i)}
}

func LogTopic(num uint64, i int) common.Hash {
	return common.Hash{0xbb, byte(num >> 8), byte(num), byte(i)}
}

// leBytes32 encodes v the way the format stores total difficulty.
func leBytes32(v *uint256.Int) []byte {
	b := v.Bytes32()
	slices.Reverse(b[:])
	return b[:]
}

// Genesis is the hash of the chain's first block, which import anchors on.
func (c *Chain) Genesis() common.Hash { return c.genesis }

// Parent is the hash of the block before num, which anchors a file that does not hold block 0.
func (c *Chain) Parent(num uint64) common.Hash { return c.hashes[num-1] }
