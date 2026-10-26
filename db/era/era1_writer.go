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
	"encoding/binary"
	"fmt"
	"io"

	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/snappypool"
)

// Block is one block as an era1 file holds it: the RLP payloads without their snappy framing, and
// the total difficulty as 32 little-endian bytes.
type Block struct {
	HeaderRLP   []byte
	BodyRLP     []byte
	ReceiptsRLP []byte
	TotalDiff   []byte
}

// Writer builds a .era1 file: a version record, each block's four records, then the accumulator and
// the index.
type Writer struct {
	w      io.Writer
	off    int64   // bytes written so far
	starts []int64 // where each block's first record begins
	first  uint64
	acc    Accumulator
}

func NewWriter(w io.Writer, firstBlock uint64) (*Writer, error) {
	e := &Writer{w: w, first: firstBlock}
	if err := e.write(typeVersion, nil); err != nil {
		return nil, err
	}
	return e, nil
}

// Add appends the next block.
func (e *Writer) Add(b Block) error {
	e.starts = append(e.starts, e.off)
	if err := e.acc.Add(crypto.Keccak256Hash(b.HeaderRLP), b.TotalDiff); err != nil {
		return err
	}
	for i, data := range [][]byte{b.HeaderRLP, b.BodyRLP, b.ReceiptsRLP} {
		var buf bytes.Buffer
		sw := snappypool.Writer(&buf)
		_, err := sw.Write(data)
		if err == nil {
			err = sw.Close()
		}
		snappypool.PutWriter(sw)
		if err == nil {
			err = e.write(blockRecords[i], buf.Bytes())
		}
		if err != nil {
			return err
		}
	}
	return e.write(typeTotalDifficulty, b.TotalDiff)
}

// Finalize writes the accumulator over the blocks added, then the index.
func (e *Writer) Finalize() error {
	root, err := e.acc.Root()
	if err != nil {
		return err
	}
	if err := e.write(typeAccumulator, root[:]); err != nil {
		return err
	}
	idx := blockIndex{startBlock: e.first, offsets: make([]int64, len(e.starts))}
	for i, at := range e.starts {
		idx.offsets[i] = at - e.off
	}
	return e.write(typeBlockIndex, idx.encode())
}

func (e *Writer) write(typ [2]byte, data []byte) error {
	var hdr [recordHeaderLen + 2]byte // room for the full uint64 length, of which 6 bytes are kept
	copy(hdr[:], typ[:])
	binary.LittleEndian.PutUint64(hdr[2:], uint64(len(data)))
	if hdr[8] != 0 || hdr[9] != 0 {
		return fmt.Errorf("era: record of %d bytes does not fit a uint48 length", len(data))
	}
	if _, err := e.w.Write(hdr[:recordHeaderLen]); err != nil {
		return err
	}
	if _, err := e.w.Write(data); err != nil {
		return err
	}
	e.off += int64(recordHeaderLen + len(data))
	return nil
}
