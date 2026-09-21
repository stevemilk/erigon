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
	"testing"

	"github.com/stretchr/testify/require"
)

// The block index has the same shape as the beacon SlotIndex, counting block numbers rather than slots:
// starting-number, one offset per block, then the count.
func TestBlockIndexRoundTrip(t *testing.T) {
	idx := blockIndex{startBlock: 1_000_000, offsets: []int64{-120, -80, 0, 64}}

	got, err := parseBlockIndex(idx.encode(), uint64(len(idx.offsets)))
	require.NoError(t, err)
	require.Equal(t, idx, got)
}

func TestBlockIndexRejectsBadCount(t *testing.T) {
	raw := blockIndex{startBlock: 7, offsets: []int64{1, 2}}.encode()
	_, err := parseBlockIndex(raw[:len(raw)-8], 2) // payload cut short
	require.Error(t, err)

	_, err = parseBlockIndex(raw, 3) // the file's count disagrees with the payload
	require.Error(t, err)

	short, err := parseBlockIndex([]byte{1, 2, 3}, 1)
	require.Error(t, err)
	require.Zero(t, short.startBlock)
}
