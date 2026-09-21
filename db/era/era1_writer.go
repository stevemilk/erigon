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
	"bytes"
	"fmt"
	"io"

	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/snappypool"
	"github.com/erigontech/erigon/db/e2store"
)

// Block is one block as it sits in an era1 file: the RLP payloads with their snappy framing
// removed, plus the total difficulty. The payloads stay encoded because that is what a segment
// writer wants.
type Block struct {
	Number      uint64
	HeaderRLP   []byte
	BodyRLP     []byte
	ReceiptsRLP []byte
	TotalDiff   []byte // uint256, little-endian as stored
}

// Block reads all of one block's components at once.
func (e *Era1) Block(num uint64) (Block, error) {
	b := Block{Number: num}
	var err error
	if b.HeaderRLP, err = e.HeaderRLP(num); err != nil {
		return Block{}, err
	}
	if b.BodyRLP, err = e.BodyRLP(num); err != nil {
		return Block{}, err
	}
	if b.ReceiptsRLP, err = e.ReceiptsRLP(num); err != nil {
		return Block{}, err
	}
	if b.TotalDiff, err = e.TotalDifficulty(num); err != nil {
		return Block{}, err
	}
	return b, nil
}

// Era1Writer builds a .era1 file: a version record, then each block's four records back to back,
// then the accumulator and the index. Blocks must be added in order.
type Era1Writer struct {
	w      io.Writer
	off    int64   // bytes written so far
	starts []int64 // where each block's first record begins
	start  uint64  // first block number
	n      uint64  // blocks added
	acc    Accumulator
}

func NewEra1Writer(w io.Writer, firstBlock uint64) (*Era1Writer, error) {
	e := &Era1Writer{w: w, start: firstBlock}
	n, err := e2store.Write(w, e2store.TypeVersion, nil)
	if err != nil {
		return nil, err
	}
	e.off = int64(n)
	return e, nil
}

// Add appends one block. The three RLP payloads are stored snappy-framed; the total difficulty is
// written as it is, the 32-byte little-endian value the format asks for.
func (e *Era1Writer) Add(b Block) error {
	if b.Number != e.start+e.n {
		return fmt.Errorf("era: blocks must be added in order: expected %d, got %d", e.start+e.n, b.Number)
	}
	e.starts = append(e.starts, e.off)
	if err := e.acc.Add(crypto.Keccak256Hash(b.HeaderRLP), b.TotalDiff); err != nil {
		return err
	}

	for _, part := range []struct {
		typ  [2]byte
		data []byte
		comp bool
	}{
		{typeCompressedHeader, b.HeaderRLP, true},
		{typeCompressedBody, b.BodyRLP, true},
		{typeCompressedReceipts, b.ReceiptsRLP, true},
		{typeTotalDifficulty, b.TotalDiff, false},
	} {
		payload := part.data
		if part.comp {
			var buf bytes.Buffer
			sw := snappypool.Writer(&buf)
			_, err := sw.Write(part.data)
			if err == nil {
				err = sw.Close()
			}
			snappypool.PutWriter(sw)
			if err != nil {
				return err
			}
			payload = buf.Bytes()
		}
		n, err := e2store.Write(e.w, part.typ, payload)
		if err != nil {
			return err
		}
		e.off += int64(n)
	}
	e.n++
	return nil
}

// Finalize writes the accumulator over the blocks added so far, then the index. The index offsets
// are relative to the start of the index record, so they are negative for every block before it.
func (e *Era1Writer) Finalize() error {
	root, err := e.acc.Root()
	if err != nil {
		return err
	}
	n, err := e2store.Write(e.w, typeAccumulator, root[:])
	if err != nil {
		return err
	}
	e.off += int64(n)

	idx := blockIndex{startBlock: e.start, offsets: make([]int64, len(e.starts))}
	for i, at := range e.starts {
		idx.offsets[i] = at - e.off
	}
	_, err = e2store.Write(e.w, typeBlockIndex, idx.encode())
	return err
}
