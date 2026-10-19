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

package era

import (
	"errors"
	"fmt"
	"slices"

	"github.com/erigontech/erigon/cl/merkle_tree"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/execution/types"
)

// Verify checks a file before its contents reach a segment. parent is the hash of the block before
// the file's first, and the hash of the last block is returned so the next file can continue.
// genesis pins block 0 when the file holds it.
//
// One of the two has to bite, or the file is only checked against itself and a fabricated chain
// passes: a file that starts the chain is pinned by genesis, and every later one by parent.
func Verify(r *Era1, parent, genesis common.Hash) (common.Hash, error) {
	if r.Start() == 0 {
		if genesis == (common.Hash{}) {
			return common.Hash{}, errors.New("era: no genesis to pin the first block on")
		}
	} else if parent == (common.Hash{}) {
		return common.Hash{}, fmt.Errorf("era: no parent hash to continue from at block %d", r.Start())
	}

	var acc Accumulator
	for num := r.Start(); num <= r.Last(); num++ {
		header, hash, err := r.Header(num)
		if err != nil {
			return common.Hash{}, err
		}
		if parent != (common.Hash{}) && header.ParentHash != parent {
			return common.Hash{}, fmt.Errorf("era: block %d follows %s, not %s", num, header.ParentHash, parent)
		}
		if num == 0 && genesis != (common.Hash{}) && hash != genesis {
			return common.Hash{}, fmt.Errorf("era: block 0 is %s, not this chain's genesis %s", hash, genesis)
		}
		parent = hash

		raw, err := r.Body(num)
		if err != nil {
			return common.Hash{}, err
		}
		// The transactions have to be decoded rather than hashed as stored: a typed one sits in the
		// body wrapped in an RLP string, and the root is taken over the unwrapped bytes.
		txns, err := types.DecodeTransactions(raw.Transactions)
		if err != nil {
			return common.Hash{}, fmt.Errorf("era: block %d body: %w", num, err)
		}
		body := types.Body{Transactions: txns, Uncles: raw.Uncles, Withdrawals: raw.Withdrawals}
		if err = body.MatchesHeader(header); err != nil {
			return common.Hash{}, fmt.Errorf("era: block %d: %w", num, err)
		}

		td, err := r.TotalDifficulty(num)
		if err != nil {
			return common.Hash{}, err
		}
		if err := acc.Add(hash, td); err != nil {
			return common.Hash{}, err
		}
	}

	stored, err := r.Accumulator()
	if err != nil {
		return common.Hash{}, err
	}
	root, err := acc.Root()
	if err != nil {
		return common.Hash{}, err
	}
	if root != stored {
		return common.Hash{}, fmt.Errorf("era: accumulator is %x, the file stores %x", root, stored)
	}
	return parent, nil
}

// Accumulator builds the file's root: the SSZ hash_tree_root of a List[HeaderRecord, 8192], each
// record pairing a block hash with the total difficulty at that block.
type Accumulator struct {
	leaves [][32]byte
}

func (a *Accumulator) Add(blockHash common.Hash, totalDifficulty []byte) error {
	if len(totalDifficulty) != 32 {
		return fmt.Errorf("era: total difficulty is %d bytes, want 32", len(totalDifficulty))
	}
	if len(a.leaves) == MaxBlocksPerFile {
		return fmt.Errorf("era: accumulator is full at %d blocks", MaxBlocksPerFile)
	}
	a.leaves = append(a.leaves, crypto.Sha256(blockHash[:], totalDifficulty))
	return nil
}

func (a *Accumulator) Root() (common.Hash, error) {
	root, err := merkle_tree.MerkleizeVector(slices.Clone(a.leaves), MaxBlocksPerFile)
	if err != nil {
		return common.Hash{}, err
	}
	length := merkle_tree.Uint64Root(uint64(len(a.leaves)))
	return crypto.Sha256(root[:], length[:]), nil
}
