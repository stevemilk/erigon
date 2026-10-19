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

package freezeblocks

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/era/eratest"
	"github.com/erigontech/erigon/db/seg"
	"github.com/erigontech/erigon/db/snapcfg"
	"github.com/erigontech/erigon/execution/chain"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
)

func requireTxnNumsContinuous(t *testing.T, dirs datadir.Dirs) {
	t.Helper()
	segs := mustGlob(t, dirs.Snap, "*-bodies-ep.seg")
	sort.Strings(segs)
	var want uint64
	for _, path := range segs {
		d, err := seg.NewDecompressor(path)
		require.NoError(t, err)
		g := d.MakeGetter()
		for g.HasNext() {
			word, _ := g.Next(nil)
			var body types.BodyForStorage
			require.NoError(t, rlp.DecodeBytes(word, &body))
			require.Equalf(t, want, uint64(body.BaseTxnID), "transaction numbering breaks at %s", filepath.Base(path))
			want = body.BaseTxnID.LastSystemTx(body.TxCount) + 1
		}
		d.Close()
	}
}

// TestImportEraTypedTransactions covers what a pre-Berlin range cannot: a typed transaction sits in
// the body wrapped in an RLP string, and the segment word keeps that wrapping, since it is what the
// database path stores too. The forks are pulled back to block 0 so the signer accepts them.
func TestImportEraTypedTransactions(t *testing.T) {
	cfg, snCfg := mainnetSpec(t)

	// Pull the forks back on the shared config and put them back afterwards: chain.Config carries a
	// sync.Once, so it cannot be copied.
	berlin, london := cfg.BerlinBlock, cfg.LondonBlock
	zero := uint64(0)
	cfg.BerlinBlock, cfg.LondonBlock = &zero, &zero
	t.Cleanup(func() { cfg.BerlinBlock, cfg.LondonBlock = berlin, london })

	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0).UseTypedTxns(cfg.ChainID.Uint64())
	chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)

	dirs := datadir.New(t.TempDir())
	require.NoError(t, ImportEra(context.Background(), eraDir, dirs, cfg, snCfg, chain.Genesis(), 1, log.New()))

	segs := mustGlob(t, dirs.Snap, "*-transactions-ep.seg")
	require.Len(t, segs, 1)
	d, err := seg.NewDecompressor(segs[0])
	require.NoError(t, err)
	defer d.Close()

	var checked int
	for g, word := d.MakeGetter(), []byte(nil); g.HasNext(); {
		word, _ = g.Next(word[:0])
		if len(word) == 0 {
			continue // system transaction
		}
		envelope := word[21:]
		require.True(t, types.TypedTransactionMarshalledAsRlpString(envelope), "the word has to keep the wrapping")
		txn, err := types.DecodeTransaction(envelope)
		require.NoError(t, err)
		require.Equal(t, byte(types.DynamicFeeTxType), txn.Type())
		require.Equal(t, txn.Hash()[0], word[0])
		checked++
	}
	require.NotZero(t, checked)
}

// TestImportEraIsIndependentOfWorkerCount pins the transaction segment against the one thing that
// could reorder it: sender recovery is spread over cores, and the words have to come out in block
// order regardless of how many run at once.
func TestImportEraIsIndependentOfWorkerCount(t *testing.T) {
	cfg, snCfg := mainnetSpec(t)

	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0)
	for range 2 {
		chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 3)
	}

	run := func(workers int) string {
		dirs := datadir.New(t.TempDir())
		require.NoError(t, ImportEra(context.Background(), eraDir, dirs, cfg, snCfg, chain.Genesis(), workers, log.New()))
		return dirs.Snap
	}

	serial, parallel := run(1), run(8)
	requireSameSegments(t, serial, parallel)
}

// TestImportEraConvertsInWindows covers the shape an operator without room for the whole archive
// has to use: fetch a few files, convert them, delete them, fetch the next few. The transaction
// count and the parent hash both come from the segments already written, so the files behind the
// window are of no further use.
func TestImportEraConvertsInWindows(t *testing.T) {
	cfg, snCfg := mainnetSpec(t)

	// Build the whole archive somewhere else, then feed it through a window two files wide.
	src := t.TempDir()
	chain := eratest.NewChain(t, 0)
	var all []string
	for range 4 {
		all = append(all, chain.WriteFile(t, src, era.MaxBlocksPerFile, 2))
	}

	window, dirs := t.TempDir(), datadir.New(t.TempDir())
	for at := 0; at < len(all); at += 2 {
		for _, stale := range mustGlob(t, window, "*.era1") {
			require.NoError(t, dir.RemoveFile(stale))
		}
		for _, f := range all[at : at+2] {
			copyFile(t, f, window)
		}
		require.NoError(t, ImportEra(context.Background(), window, dirs, cfg, snCfg, chain.Genesis(), 1, log.New()))
	}

	require.Len(t, mustGlob(t, dirs.Snap, "*-headers-ep.seg"), len(all))
	requireTxnNumsContinuous(t, dirs)

	// And the result is what converting the whole archive at once would have produced.
	whole := datadir.New(t.TempDir())
	require.NoError(t, ImportEra(context.Background(), src, whole, cfg, snCfg, chain.Genesis(), 1, log.New()))
	requireSameSegments(t, whole.Snap, dirs.Snap)
}

// rangeOf is the from-to part of a segment name.
func rangeOf(segName string) string {
	parts := strings.Split(segName, "-")
	return parts[1] + "-" + parts[2]
}

func mustGlob(t *testing.T, dir, pat string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, pat))
	require.NoError(t, err)
	return m
}

func copyFile(t *testing.T, src, dstDir string) {
	t.Helper()
	copyFileAs(t, src, filepath.Join(dstDir, filepath.Base(src)))
}

func copyFileAs(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dst, data, 0o644))
}

func requireSameSegments(t *testing.T, want, got string) {
	t.Helper()
	segs := mustGlob(t, want, "*-ep.seg")
	require.NotEmpty(t, segs, "nothing to compare")
	require.Len(t, mustGlob(t, got, "*-ep.seg"), len(segs), "the two runs produced different file sets")
	for _, src := range segs {
		a, err := os.ReadFile(src)
		require.NoError(t, err)
		b, err := os.ReadFile(filepath.Join(got, filepath.Base(src)))
		require.NoError(t, err)
		require.Truef(t, bytes.Equal(a, b), "%s differs", filepath.Base(src))
	}
}

// mainnetSpec is the chain every test here converts against.
func mainnetSpec(t *testing.T) (*chain.Config, *snapcfg.Cfg) {
	t.Helper()
	spec, err := chainspec.ChainSpecByName("mainnet")
	require.NoError(t, err)
	snCfg, known := snapcfg.KnownCfg(spec.Config.ChainName)
	require.True(t, known)
	return spec.Config, snCfg
}

// importEra converts eraDir into dirs, with one worker so the tests stay quick.
func importEra(t *testing.T, eraDir string, dirs datadir.Dirs, chain *eratest.Chain) error {
	t.Helper()
	cfg, snCfg := mainnetSpec(t)
	return ImportEra(context.Background(), eraDir, dirs, cfg, snCfg, chain.Genesis(), 1, log.New())
}

// TestImportEraResumes covers picking up where an earlier run stopped. Neither the transaction
// count nor the parent hash is in the archive, so both have to come back out of the segments.
func TestImportEraResumes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []int // blocks per archive file
	}{
		{"on a file boundary", []int{era.MaxBlocksPerFile, era.MaxBlocksPerFile, era.MaxBlocksPerFile}},
		// A short last file tiles the tail below one file, so the segments past the last file
		// boundary are rebuilt in place.
		{"with a partial last file", []int{era.MaxBlocksPerFile, era.MaxBlocksPerFile, 3000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eraDir := t.TempDir()
			chain := eratest.NewChain(t, 0)
			for _, n := range tc.files {
				chain.WriteFile(t, eraDir, n, 1)
			}

			whole := datadir.New(t.TempDir())
			require.NoError(t, importEra(t, eraDir, whole, chain))
			built := mustGlob(t, whole.Snap, "*-headers-ep.seg")
			require.Greater(t, len(built), 2, "the run has to be split for a resume to be exercised")

			// Leave everything but the last range, as an interrupted run would.
			partial := datadir.New(t.TempDir())
			last := rangeOf(filepath.Base(built[len(built)-1]))
			for _, src := range mustGlob(t, whole.Snap, "*-ep.*") {
				if !strings.Contains(filepath.Base(src), last) {
					copyFile(t, src, partial.Snap)
				}
			}

			require.NoError(t, importEra(t, eraDir, partial, chain))
			requireSameSegments(t, whole.Snap, partial.Snap)
		})
	}
}

// TestImportEraRejects covers the ways a datadir or an archive can fail to continue the run, all of
// which would otherwise produce segments that look fine and are not.
func TestImportEraRejects(t *testing.T) {
	cfg, snCfg := mainnetSpec(t)

	t.Run("a window from another chain", func(t *testing.T) {
		// Well formed, correctly named, covering exactly the blocks the window wants next — but its
		// first block holds no block 0, so only the hash carried over from the segments catches it.
		dirs := datadir.New(t.TempDir())
		ours := eratest.NewChain(t, 0)
		oursDir := t.TempDir()
		ours.WriteFile(t, oursDir, era.MaxBlocksPerFile, 1)
		require.NoError(t, importEra(t, oursDir, dirs, ours))

		theirs, theirsDir, window := eratest.NewChain(t, 0), t.TempDir(), t.TempDir()
		theirs.WriteFile(t, theirsDir, era.MaxBlocksPerFile, 1)
		copyFile(t, theirs.WriteFile(t, theirsDir, era.MaxBlocksPerFile, 1), window)

		err := ImportEra(context.Background(), window, dirs, cfg, snCfg, ours.Genesis(), 1, log.New())
		require.ErrorContains(t, err, "follows")
	})

	t.Run("a window that skips past the segments", func(t *testing.T) {
		dirs := datadir.New(t.TempDir())
		src, c := t.TempDir(), eratest.NewChain(t, 0)
		var all []string
		for range 3 {
			all = append(all, c.WriteFile(t, src, era.MaxBlocksPerFile, 1))
		}
		window := t.TempDir()
		copyFile(t, all[0], window)
		require.NoError(t, importEra(t, window, dirs, c))

		require.NoError(t, dir.RemoveFile(filepath.Join(window, filepath.Base(all[0]))))
		copyFile(t, all[2], window) // the segments end at 8192, the archive now starts at 16384
		require.ErrorContains(t, importEra(t, window, dirs, c), "the archive starts at")
	})
}
