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

// Package era reads .era1 archive files: e2store records holding one epoch of pre-merge blocks
// keyed by block number.
//
// https://github.com/eth-clients/e2store-format-specs
package era

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// MaxBlocksPerFile is the epoch length, and the size of the accumulator list.
const MaxBlocksPerFile = 8192

// Archive is an unbroken run of one network's .era1 files in a directory. It keeps the file last
// asked for open, since callers walk block numbers in order.
type Archive struct {
	first int // epoch of paths[0]
	paths []string
	open  *Era1
	openN int
}

func OpenArchive(dir, network string) (*Archive, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	byEpoch := map[int]string{}
	for _, e := range entries {
		epoch, ok := parseName(e.Name(), network)
		if !ok {
			continue
		}
		if prev, dup := byEpoch[epoch]; dup {
			return nil, fmt.Errorf("era: epoch %d is in both %s and %s", epoch, prev, e.Name())
		}
		byEpoch[epoch] = filepath.Join(dir, e.Name())
	}
	if len(byEpoch) == 0 {
		return nil, fmt.Errorf("era: no %s .era1 files in %s", network, dir)
	}
	epochs := slices.Sorted(maps.Keys(byEpoch))
	a := &Archive{first: epochs[0], openN: -1}
	for i, epoch := range epochs {
		if epoch != a.first+i {
			return nil, fmt.Errorf("era: epoch %d is missing; the files have to be an unbroken run", a.first+i)
		}
		a.paths = append(a.paths, byEpoch[epoch])
	}
	return a, nil
}

// From is the first block the archive holds.
func (a *Archive) From() uint64 { return uint64(a.first) * MaxBlocksPerFile }

// End is the block after the last one the archive holds.
func (a *Archive) End() (uint64, error) {
	f, err := a.File(a.From() + uint64(len(a.paths)-1)*MaxBlocksPerFile)
	if err != nil {
		return 0, err
	}
	return f.Last() + 1, nil
}

// File returns the file holding block num. It stays valid until the next call or Close.
func (a *Archive) File(num uint64) (*Era1, error) {
	n := int(num/MaxBlocksPerFile) - a.first
	if n < 0 || n >= len(a.paths) {
		return nil, fmt.Errorf("era: block %d is outside the archive", num)
	}
	if n == a.openN {
		return a.open, nil
	}
	a.Close()
	f, err := OpenEra1(a.paths[n])
	if err != nil {
		return nil, err
	}
	// Callers trust the name's epoch to place a file, so a file must hold the epoch it is named for.
	if want := uint64(a.first+n) * MaxBlocksPerFile; f.Start() != want {
		f.Close()
		return nil, fmt.Errorf("era: %s starts at block %d, not %d", filepath.Base(a.paths[n]), f.Start(), want)
	}
	a.open, a.openN = f, n
	return f, nil
}

func (a *Archive) Close() {
	if a.open != nil {
		a.open.Close()
		a.open, a.openN = nil, -1
	}
}

// parseName reads the epoch out of <network>-<epoch>-<tag>.era1. The tag is not checked: what a file
// holds is settled by verifying it, not by its name.
func parseName(name, network string) (int, bool) {
	rest, ok := strings.CutPrefix(name, network+"-")
	if !ok || filepath.Ext(rest) != ".era1" {
		return 0, false
	}
	epoch, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(epoch)
	return n, err == nil
}
