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

package e2store

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

var (
	testTypeA = [2]byte{0x03, 0x00}
	testTypeB = [2]byte{0x04, 0x00}
)

// A record is a 2-byte type, a little-endian uint48 length, then the payload. Readers seek to a
// record and take its header, so that is the pairing worth pinning.
func TestRecordRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("header bytes")

	n, err := Write(&buf, testTypeA, payload)
	require.NoError(t, err)
	require.Equal(t, HeaderLen+len(payload), n)

	typ, size, err := ReadHeaderAt(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)
	require.Equal(t, testTypeA, typ)
	require.Equal(t, int64(len(payload)), size)
	require.Equal(t, payload, buf.Bytes()[HeaderLen:])
}

// Records are written back to back, so a reader walks them by adding up the sizes.
func TestRecordsAreContiguous(t *testing.T) {
	var buf bytes.Buffer
	for _, payload := range [][]byte{nil, []byte("one"), []byte("two")} {
		_, err := Write(&buf, testTypeB, payload)
		require.NoError(t, err)
	}

	r := bytes.NewReader(buf.Bytes())
	var at int64
	for _, want := range []int64{0, 3, 3} {
		typ, size, err := ReadHeaderAt(r, at)
		require.NoError(t, err)
		require.Equal(t, testTypeB, typ)
		require.Equal(t, want, size)
		at += HeaderLen + size
	}
	require.Equal(t, int64(buf.Len()), at)
}

func TestReadHeaderRejectsAShortFile(t *testing.T) {
	var buf bytes.Buffer
	_, err := Write(&buf, testTypeA, []byte("four"))
	require.NoError(t, err)

	_, _, err = ReadHeaderAt(bytes.NewReader(buf.Bytes()[:HeaderLen-1]), 0)
	require.Error(t, err)
}

// An empty payload is legal: the version record carries none.
func TestVersionRecordHasNoPayload(t *testing.T) {
	var buf bytes.Buffer
	_, err := Write(&buf, TypeVersion, nil)
	require.NoError(t, err)
	require.Equal(t, HeaderLen, buf.Len())

	typ, size, err := ReadHeaderAt(bytes.NewReader(buf.Bytes()), 0)
	require.NoError(t, err)
	require.Equal(t, TypeVersion, typ)
	require.Zero(t, size)
}
