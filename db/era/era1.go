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
	"os"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/length"
	"github.com/erigontech/erigon/common/snappypool"
	"github.com/erigontech/erigon/db/e2store"
)

// era1 record types.
var (
	typeCompressedHeader   = [2]byte{0x03, 0x00}
	typeCompressedBody     = [2]byte{0x04, 0x00}
	typeCompressedReceipts = [2]byte{0x05, 0x00}
	typeTotalDifficulty    = [2]byte{0x06, 0x00}
	typeAccumulator        = [2]byte{0x07, 0x00}
	typeBlockIndex         = [2]byte{0x66, 0x32}
)

// blockRecords is the complete set of a block's records, in the order they are written. The scan in
// component is bounded by it.
var blockRecords = [4][2]byte{typeCompressedHeader, typeCompressedBody, typeCompressedReceipts, typeTotalDifficulty}

// Era1 reads blocks out of a .era1 file by number, using the index at the end of the file.
type Era1 struct {
	f     *os.File
	size  int64
	idx   blockIndex
	idxAt int64 // where the index record starts; offsets are measured from here
}

var _ Reader = (*Era1)(nil)

// OpenEra1 reads the trailing index so blocks can be fetched by number without scanning.
func OpenEra1(path string) (*Era1, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	e := &Era1{f: f}
	if err := e.readIndex(); err != nil {
		f.Close()
		return nil, err
	}
	return e, nil
}

func (e *Era1) Close() error  { return e.f.Close() }
func (e *Era1) Start() uint64 { return e.idx.startBlock }
func (e *Era1) Count() int    { return len(e.idx.offsets) }
func (e *Era1) Last() uint64  { return e.idx.startBlock + uint64(len(e.idx.offsets)) - 1 }

func (e *Era1) HeaderRLP(num uint64) ([]byte, error) {
	return e.decompressed(num, typeCompressedHeader)
}

func (e *Era1) BodyRLP(num uint64) ([]byte, error) {
	return e.decompressed(num, typeCompressedBody)
}

func (e *Era1) ReceiptsRLP(num uint64) ([]byte, error) {
	return e.decompressed(num, typeCompressedReceipts)
}

func (e *Era1) TotalDifficulty(num uint64) ([]byte, error) {
	return e.component(num, typeTotalDifficulty)
}

// Accumulator returns the root, which is stored in the fixed-size record just before the index.
func (e *Era1) Accumulator() (common.Hash, error) {
	rec, err := e.recordAt(e.idxAt - e2store.HeaderLen - length.Hash)
	if err != nil {
		return common.Hash{}, err
	}
	if rec.Type != typeAccumulator || len(rec.Data) != length.Hash {
		return common.Hash{}, fmt.Errorf("era: no accumulator before the index: type %x, %d bytes", rec.Type, len(rec.Data))
	}
	return common.BytesToHash(rec.Data), nil
}

// readIndex locates the index, which is always the last record: the block count sits in the final 8
// bytes and gives the payload length, which gives the record start.
func (e *Era1) readIndex() error {
	info, err := e.f.Stat()
	if err != nil {
		return err
	}
	e.size = info.Size()
	if e.size < e2store.HeaderLen+16 {
		return fmt.Errorf("era: file is %d bytes, too short to hold an index", e.size)
	}
	var tail [8]byte
	if _, err := e.f.ReadAt(tail[:], e.size-8); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint64(tail[:])
	if count == 0 || count > MaxBlocksPerFile {
		return fmt.Errorf("era: index claims %d blocks, outside 1..%d", count, MaxBlocksPerFile)
	}
	e.idxAt = e.size - int64(16+count*8) - e2store.HeaderLen
	if e.idxAt < 0 {
		return fmt.Errorf("era: index claims %d blocks, which does not fit in %d bytes", count, e.size)
	}

	rec, err := e.recordAt(e.idxAt)
	if err != nil {
		return err
	}
	if rec.Type != typeBlockIndex {
		return fmt.Errorf("era: last record is type %x, not a block index", rec.Type)
	}
	e.idx, err = parseBlockIndex(rec.Data, count)
	return err
}

func (e *Era1) recordAt(off int64) (e2store.Record, error) {
	typ, size, err := e2store.ReadHeaderAt(e.f, off)
	if err != nil {
		return e2store.Record{}, err
	}
	data, err := e.payloadAt(off+e2store.HeaderLen, size)
	if err != nil {
		return e2store.Record{}, err
	}
	return e2store.Record{Type: typ, Data: data}, nil
}

// payloadAt reads size bytes, refusing a length the file cannot hold.
func (e *Era1) payloadAt(off, size int64) ([]byte, error) {
	if size < 0 || off+size > e.size {
		return nil, fmt.Errorf("era: record at %d claims %d bytes, past the end of a %d byte file", off, size, e.size)
	}
	data := make([]byte, size)
	if _, err := e.f.ReadAt(data, off); err != nil {
		return nil, err
	}
	return data, nil
}

// component reads the payload of one of a block's records, skipping over the ones before it.
func (e *Era1) component(num uint64, want [2]byte) ([]byte, error) {
	i := int(num - e.idx.startBlock)
	if num < e.idx.startBlock || i >= len(e.idx.offsets) {
		return nil, fmt.Errorf("era: block %d is outside [%d,%d]", num, e.Start(), e.Last())
	}

	off := e.idxAt + e.idx.offsets[i]
	for range blockRecords {
		typ, size, err := e2store.ReadHeaderAt(e.f, off)
		if err != nil {
			return nil, err
		}
		if typ == want {
			return e.payloadAt(off+e2store.HeaderLen, size)
		}
		off += e2store.HeaderLen + size
	}
	return nil, fmt.Errorf("era: block %d has no record of type %x", num, want)
}

func (e *Era1) decompressed(num uint64, want [2]byte) ([]byte, error) {
	payload, err := e.component(num, want)
	if err != nil {
		return nil, err
	}
	sr := snappypool.Reader(bytes.NewReader(payload))
	defer snappypool.PutReader(sr)
	out, err := io.ReadAll(sr)
	if err != nil {
		return nil, fmt.Errorf("era: block %d: decompress %x: %w", num, want, err)
	}
	return out, nil
}

// blockIndex is the decoded index record, laid out as
// starting-number [8 LE] | offset[count] [8 LE each] | count [8 LE].
// Offsets are measured from the start of the index record, so they are negative.
type blockIndex struct {
	startBlock uint64
	offsets    []int64
}

// parseBlockIndex decodes the payload against the count readIndex already validated. The payload
// repeats that count in its last 8 bytes, but those are different bytes on disk and a crafted file
// can disagree, so the validated one is what the payload has to match.
func parseBlockIndex(data []byte, count uint64) (blockIndex, error) {
	const fixed = 16 // starting-number + count
	if want := fixed + count*8; uint64(len(data)) != want {
		return blockIndex{}, fmt.Errorf("era: block index of %d blocks needs %d bytes, but the payload is %d", count, want, len(data))
	}
	if repeated := binary.LittleEndian.Uint64(data[len(data)-8:]); repeated != count {
		return blockIndex{}, fmt.Errorf("era: block index says %d blocks, the file says %d", repeated, count)
	}
	start := binary.LittleEndian.Uint64(data[:8])

	offsets := make([]int64, count)
	for i := range offsets {
		offsets[i] = int64(binary.LittleEndian.Uint64(data[8+i*8 : 16+i*8]))
	}
	return blockIndex{startBlock: start, offsets: offsets}, nil
}

func (b blockIndex) encode() []byte {
	out := make([]byte, 16+len(b.offsets)*8)
	binary.LittleEndian.PutUint64(out[:8], b.startBlock)
	for i, off := range b.offsets {
		binary.LittleEndian.PutUint64(out[8+i*8:16+i*8], uint64(off))
	}
	binary.LittleEndian.PutUint64(out[len(out)-8:], uint64(len(b.offsets)))
	return out
}
