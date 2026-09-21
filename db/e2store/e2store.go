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

// Package e2store implements the TLV container shared by the .era, .era1 and .ere archive formats:
// a stream of records, each a 2-byte type tag, a little-endian uint48 length, and that many bytes.
// Only the record types and the payload encoding differ between the formats.
//
// https://github.com/eth-clients/e2store-format-specs
package e2store

import (
	"encoding/binary"
	"fmt"
	"io"
)

// TypeVersion opens every e2store file and carries an empty payload.
var TypeVersion = [2]byte{0x65, 0x32}

// HeaderLen is the fixed record header: type[2] + length[6 LE].
const HeaderLen = 8

// Record is one TLV record.
type Record struct {
	Type [2]byte
	Data []byte
}

// ReadHeaderAt reads just a record header, for readers that seek by offset and skip payloads they
// don't want.
func ReadHeaderAt(r io.ReaderAt, off int64) (typ [2]byte, length int64, err error) {
	var hdr [HeaderLen]byte
	if _, err := r.ReadAt(hdr[:], off); err != nil {
		return typ, 0, err
	}
	var lenBuf [8]byte
	copy(lenBuf[:6], hdr[2:8])
	return [2]byte{hdr[0], hdr[1]}, int64(binary.LittleEndian.Uint64(lenBuf[:])), nil
}

// Write writes one record and returns its size, header included.
func Write(w io.Writer, typ [2]byte, data []byte) (int, error) {
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(data)))
	if lenBuf[6] != 0 || lenBuf[7] != 0 {
		return 0, fmt.Errorf("e2store: record of %d bytes does not fit the uint48 length field", len(data))
	}

	var hdr [HeaderLen]byte
	hdr[0], hdr[1] = typ[0], typ[1]
	copy(hdr[2:8], lenBuf[:6])

	if _, err := w.Write(hdr[:]); err != nil {
		return 0, err
	}
	if _, err := w.Write(data); err != nil {
		return HeaderLen, err
	}
	return HeaderLen + len(data), nil
}
