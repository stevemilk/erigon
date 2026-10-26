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
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/length"
	"github.com/erigontech/erigon/common/snappypool"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
)

// Record types. Every e2store record is a 2-byte type, a little-endian uint48 length, and the payload.
var (
	typeVersion            = [2]byte{0x65, 0x32}
	typeCompressedHeader   = [2]byte{0x03, 0x00}
	typeCompressedBody     = [2]byte{0x04, 0x00}
	typeCompressedReceipts = [2]byte{0x05, 0x00}
	typeTotalDifficulty    = [2]byte{0x06, 0x00}
	typeAccumulator        = [2]byte{0x07, 0x00}
	typeBlockIndex         = [2]byte{0x66, 0x32}
)

const recordHeaderLen = 8

// blockRecords are a block's records in the order they are written.
var blockRecords = [4][2]byte{typeCompressedHeader, typeCompressedBody, typeCompressedReceipts, typeTotalDifficulty}

// Era1 reads blocks out of a .era1 file by number, using the index at the end of the file. It reads
// at offsets rather than through a cursor, so concurrent reads are safe.
type Era1 struct {
	f     *os.File
	size  int64
	idx   blockIndex
	idxAt int64 // where the index record starts; offsets are measured from here
}

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

func (e *Era1) BodyRLP(num uint64) ([]byte, error) { return e.decompressed(num, typeCompressedBody) }

func (e *Era1) ReceiptsRLP(num uint64) ([]byte, error) {
	return e.decompressed(num, typeCompressedReceipts)
}

// TotalDifficulty returns the stored 32-byte little-endian value.
func (e *Era1) TotalDifficulty(num uint64) ([]byte, error) {
	return e.component(num, typeTotalDifficulty)
}

// Header returns block num's header and its hash.
func (e *Era1) Header(num uint64) (*types.Header, common.Hash, error) {
	raw, err := e.HeaderRLP(num)
	if err != nil {
		return nil, common.Hash{}, err
	}
	var h types.Header
	if err := rlp.DecodeBytes(raw, &h); err != nil {
		return nil, common.Hash{}, fmt.Errorf("era: block %d header: %w", num, err)
	}
	return &h, crypto.Keccak256Hash(raw), nil
}

// Body returns block num's body with the transactions left encoded, as segments store them.
func (e *Era1) Body(num uint64) (*types.RawBody, error) {
	raw, err := e.BodyRLP(num)
	if err != nil {
		return nil, err
	}
	var b types.RawBody
	if err := rlp.DecodeBytes(raw, &b); err != nil {
		return nil, fmt.Errorf("era: block %d body: %w", num, err)
	}
	return &b, nil
}

// Receipts returns block num's consensus receipts, without the fields derived from the block.
func (e *Era1) Receipts(num uint64) (types.Receipts, error) {
	raw, err := e.ReceiptsRLP(num)
	if err != nil {
		return nil, err
	}
	var r types.Receipts
	if err := rlp.DecodeBytes(raw, &r); err != nil {
		return nil, fmt.Errorf("era: block %d receipts: %w", num, err)
	}
	return r, nil
}

// Accumulator returns the root, which is stored in the fixed-size record just before the index.
func (e *Era1) Accumulator() (common.Hash, error) {
	typ, data, err := e.recordAt(e.idxAt - recordHeaderLen - length.Hash)
	if err != nil {
		return common.Hash{}, err
	}
	if typ != typeAccumulator || len(data) != length.Hash {
		return common.Hash{}, fmt.Errorf("era: no accumulator before the index: type %x, %d bytes", typ, len(data))
	}
	return common.BytesToHash(data), nil
}

// readIndex locates the index, which is always the last record: the block count sits in the final 8
// bytes and gives the payload length, which gives the record start.
func (e *Era1) readIndex() error {
	info, err := e.f.Stat()
	if err != nil {
		return err
	}
	e.size = info.Size()
	if e.size < recordHeaderLen+16 {
		return fmt.Errorf("era: file is %d bytes, too short to hold an index", e.size)
	}
	var tail [8]byte
	if _, err = e.f.ReadAt(tail[:], e.size-8); err != nil {
		return err
	}
	count := binary.LittleEndian.Uint64(tail[:])
	if count == 0 || count > MaxBlocksPerFile {
		return fmt.Errorf("era: index claims %d blocks, outside 1..%d", count, MaxBlocksPerFile)
	}
	e.idxAt = e.size - int64(16+count*8) - recordHeaderLen
	if e.idxAt < 0 {
		return fmt.Errorf("era: index claims %d blocks, which does not fit in %d bytes", count, e.size)
	}
	typ, data, err := e.recordAt(e.idxAt)
	if err != nil {
		return err
	}
	if typ != typeBlockIndex {
		return fmt.Errorf("era: last record is type %x, not a block index", typ)
	}
	e.idx, err = parseBlockIndex(data, count)
	return err
}

func (e *Era1) headerAt(off int64) (typ [2]byte, size int64, err error) {
	var hdr [recordHeaderLen]byte
	if _, err := e.f.ReadAt(hdr[:], off); err != nil {
		return typ, 0, err
	}
	var lenBuf [8]byte
	copy(lenBuf[:6], hdr[2:])
	return [2]byte{hdr[0], hdr[1]}, int64(binary.LittleEndian.Uint64(lenBuf[:])), nil
}

func (e *Era1) recordAt(off int64) (typ [2]byte, data []byte, err error) {
	typ, size, err := e.headerAt(off)
	if err != nil {
		return typ, nil, err
	}
	data, err = e.payloadAt(off+recordHeaderLen, size)
	return typ, data, err
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
		typ, size, err := e.headerAt(off)
		if err != nil {
			return nil, err
		}
		if typ == want {
			return e.payloadAt(off+recordHeaderLen, size)
		}
		off += recordHeaderLen + size
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
	if want := 16 + count*8; uint64(len(data)) != want {
		return blockIndex{}, fmt.Errorf("era: block index of %d blocks needs %d bytes, but the payload is %d", count, want, len(data))
	}
	if repeated := binary.LittleEndian.Uint64(data[len(data)-8:]); repeated != count {
		return blockIndex{}, fmt.Errorf("era: block index says %d blocks, the file says %d", repeated, count)
	}
	offsets := make([]int64, count)
	for i := range offsets {
		offsets[i] = int64(binary.LittleEndian.Uint64(data[8+i*8:]))
	}
	return blockIndex{startBlock: binary.LittleEndian.Uint64(data), offsets: offsets}, nil
}

func (b blockIndex) encode() []byte {
	out := make([]byte, 16+len(b.offsets)*8)
	binary.LittleEndian.PutUint64(out, b.startBlock)
	for i, off := range b.offsets {
		binary.LittleEndian.PutUint64(out[8+i*8:], uint64(off))
	}
	binary.LittleEndian.PutUint64(out[len(out)-8:], uint64(len(b.offsets)))
	return out
}
