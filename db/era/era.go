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

// Package era reads the execution-layer archive formats built on e2store: .era1 and its successor
// .ere, which also covers post-merge history. Both hold one epoch of 8192 blocks keyed by block
// number and differ only in how the records are laid out and indexed.
package era

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/erigontech/erigon/common"
)

// MaxBlocksPerFile is the epoch length both formats use, and the size of the accumulator list.
const MaxBlocksPerFile = 8192

// Reader gives access to one archive file by block number. Receipts are left out: the two formats
// encode them differently and execution regenerates them anyway.
//
// Implementations must be safe for concurrent reads — conversion spreads a range of blocks over
// cores — so they have to address the file rather than carry a cursor.
type Reader interface {
	Start() uint64
	Count() int
	Last() uint64

	HeaderRLP(num uint64) ([]byte, error)
	BodyRLP(num uint64) ([]byte, error)

	// TotalDifficulty returns the stored 32-byte little-endian value.
	TotalDifficulty(num uint64) ([]byte, error)

	Accumulator() (common.Hash, error)

	Close() error
}

// Open opens an archive file, choosing the format from its extension.
func Open(path string) (Reader, error) {
	switch ext := filepath.Ext(path); ext {
	case ".era1":
		return OpenEra1(path)
	case ".ere":
		return nil, fmt.Errorf("era: .ere is not implemented yet: %s", path)
	default:
		return nil, fmt.Errorf("era: %q is not an archive file extension", ext)
	}
}

// Name is an archive file name, which the formats spell <network>-<epoch>-<tag>. The tag is the
// first four bytes of the accumulator root, and is not read: what a file holds is settled by
// verifying it, not by what it is called.
type Name struct {
	Network string
	Epoch   int
}

func ParseName(path string) (Name, bool) {
	base := filepath.Base(path)
	// Split from the right: the last two parts are the epoch and the tag, and a network name can
	// itself contain a hyphen.
	parts := strings.Split(strings.TrimSuffix(base, filepath.Ext(base)), "-")
	if len(parts) < 3 {
		return Name{}, false
	}
	epoch, err := strconv.Atoi(parts[len(parts)-2])
	if err != nil {
		return Name{}, false
	}
	return Name{Network: strings.Join(parts[:len(parts)-2], "-"), Epoch: epoch}, true
}
