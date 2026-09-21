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
	"fmt"

	"github.com/erigontech/erigon/cl/merkle_tree"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
)

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
	leaves := make([][32]byte, len(a.leaves))
	copy(leaves, a.leaves)
	root, err := merkle_tree.MerkleizeVector(leaves, MaxBlocksPerFile)
	if err != nil {
		return common.Hash{}, err
	}
	length := merkle_tree.Uint64Root(uint64(len(a.leaves)))
	return crypto.Sha256(root[:], length[:]), nil
}
