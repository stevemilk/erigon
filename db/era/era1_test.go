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
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRealEra1RoundTrip reads a real .era1 file and writes it back out, including an accumulator the
// writer derives itself. Anything wrong with the framing, the compression, the index or the
// accumulator shows up as a byte difference against the file we started from. Set ERA1_FILE to run.
func TestRealEra1RoundTrip(t *testing.T) {
	path := os.Getenv("ERA1_FILE")
	if path == "" {
		t.Skip("set ERA1_FILE to run")
	}
	orig, err := os.ReadFile(path)
	require.NoError(t, err)

	r, err := OpenEra1(path)
	require.NoError(t, err)
	defer r.Close()

	var out bytes.Buffer
	w, err := NewEra1Writer(&out, r.Start())
	require.NoError(t, err)
	for n := r.Start(); n <= r.Last(); n++ {
		b, err := r.Block(n)
		require.NoError(t, err)
		require.NoError(t, w.Add(b))
	}
	require.NoError(t, w.Finalize())

	require.Equal(t, len(orig), out.Len(), "file length differs")
	require.True(t, bytes.Equal(orig, out.Bytes()), "rewritten file differs from the original")
	t.Logf("rewrote blocks %d..%d byte for byte (%d bytes)", r.Start(), r.Last(), out.Len())
}
