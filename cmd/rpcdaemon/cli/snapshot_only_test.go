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

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/cmd/rpcdaemon/cli/httpcfg"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx/mdbxtest"
	"github.com/erigontech/erigon/db/kv/rawdbv3"
	"github.com/erigontech/erigon/db/kv/temporal"
	"github.com/erigontech/erigon/db/rawdb/rawtemporaldb"
	dbstate "github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/db/version"
	"github.com/erigontech/erigon/execution/chain/networkname"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/execfinality"
)

func TestReceiptsFrontier(t *testing.T) {
	t.Parallel()
	// Block b owns txNums [10b, 10b+9]; txNum 10b+9 is its block-end system tx, which has no receipt.
	const lastBlock = 20
	blockOf := func(txNum uint64) (uint64, error) {
		if txNum/10 > lastBlock {
			return 0, fmt.Errorf("no block holds txNum %d", txNum)
		}
		return txNum / 10, nil
	}

	tests := []struct {
		name         string
		rcacheEnd    uint64
		wantFrontier uint64
		wantReceipts bool
	}{
		{"no rcache files", 0, lastBlock, false},
		{"rcache ends with a block-end write", 49, 4, true},
		{"rcache ends with the first block-end write", 9, 0, true},
		{"rcache ends inside a block", 55, 4, true},
		{"rcache ends inside the first block", 5, lastBlock, false},
		{"rcache ends with the last block-end write", lastBlock*10 + 9, lastBlock, true},
		{"rcache reaches past the block files", 1000, lastBlock, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frontier, receipts, err := receiptsFrontier(lastBlock, lastBlock*10+9, tt.rcacheEnd, blockOf)
			require.NoError(t, err)
			require.Equal(t, tt.wantFrontier, frontier)
			require.Equal(t, tt.wantReceipts, receipts)
		})
	}
}

func TestSnapshotOnlySpec(t *testing.T) {
	t.Parallel()
	mainnet, err := chainspec.ChainSpecByName(networkname.Mainnet)
	require.NoError(t, err)
	hoodi, err := chainspec.ChainSpecByName(networkname.Hoodi)
	require.NoError(t, err)

	tests := []struct {
		name      string
		chainFlag string
		genesis   common.Hash
		want      string
		wantErr   string
	}{
		{"inferred from block 0", "", hoodi.GenesisHash, networkname.Hoodi, ""},
		{"--chain matching block 0", networkname.Mainnet, mainnet.GenesisHash, networkname.Mainnet, ""},
		{"--chain of another chain", networkname.Hoodi, mainnet.GenesisHash, "", "genesis"},
		{"chains sharing a genesis need --chain", "", mainnet.GenesisHash, "", "--chain"},
		{"unknown genesis", "", common.Hash{1}, "", "not the genesis of a known chain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := snapshotOnlySpec(tt.chainFlag, tt.genesis)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, spec.Name)
		})
	}
}

func TestSnapshotOnlyStateDirs(t *testing.T) {
	t.Parallel()
	salt := func(d datadir.Dirs) {
		require.NoError(t, os.MkdirAll(d.Snap, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(d.Snap, "salt-state.txt"), []byte{1, 2, 3, 4}, 0o644))
	}
	settings := func(d datadir.Dirs) {
		require.NoError(t, os.WriteFile(filepath.Join(d.Snap, dbstate.ERIGONDB_SETTINGS_FILE), []byte("step_size = 64\n"), 0o644))
	}
	stateDirs := func(dirs datadir.Dirs) datadir.Dirs {
		d, cleanup, err := snapshotOnlyStateDirs(dirs, log.New())
		require.NoError(t, err)
		t.Cleanup(cleanup)
		return d
	}

	dirs := datadir.New(t.TempDir())
	none := stateDirs(dirs)
	require.NotEqual(t, dirs.Snap, none.Snap, "no receipt cache anywhere: blocks only, nothing written to the datadir")
	require.FileExists(t, filepath.Join(none.Snap, "salt-state.txt"))
	require.NoFileExists(t, filepath.Join(dirs.Snap, "salt-state.txt"))

	require.NoError(t, os.MkdirAll(dirs.EraReceipts().Snap, 0o755))
	salt(dirs)
	_, _, err := snapshotOnlyStateDirs(dirs, log.New())
	require.ErrorContains(t, err, dbstate.ERIGONDB_SETTINGS_FILE, "the step size would have to be guessed")
	require.NoFileExists(t, filepath.Join(dirs.Snap, dbstate.ERIGONDB_SETTINGS_FILE), "nothing is written to the datadir")

	settings(dirs)
	require.Equal(t, dirs.Snap, stateDirs(dirs).Snap, "an import left without salt does not hide a node's own files")

	salt(dirs.EraReceipts())
	settings(dirs.EraReceipts())
	require.Equal(t, dirs.EraReceipts().Snap, stateDirs(dirs).Snap, "receipts imported from era1")
}

func TestConfigureSnapshotOnly(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	snapDir := filepath.Join(dataDir, datadir.SnapDir)

	cfg := &httpcfg.HttpCfg{}
	require.NoError(t, ConfigureSnapshotOnly(cfg, snapDir, ":8545", networkname.Hoodi))
	require.Equal(t, dataDir, cfg.Dirs.DataDir, "the lock and the era receipts sit beside the snapshots")
	require.Equal(t, snapDir, cfg.Dirs.Snap)
	require.Equal(t, "", cfg.HttpListenAddress)
	require.Equal(t, 8545, cfg.HttpPort)
	require.Equal(t, networkname.Hoodi, cfg.Snap.ChainName)

	require.ErrorContains(t, ConfigureSnapshotOnly(cfg, dataDir, ":8545", ""), datadir.SnapDir, "a datadir is not its snapshot directory")
	require.ErrorContains(t, ConfigureSnapshotOnly(cfg, snapDir, "8545", ""), "--listen")
}

func TestRcacheHistoryEnd(t *testing.T) {
	tests := []struct {
		name        string
		missingStep string
		lateStart   bool
	}{
		{"contiguous files", "", false},
		{"files start late", "0-1", true},
		{"gap in the files", "2-3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirs := datadir.New(t.TempDir())
			buildRcacheFiles(t, dirs)
			if tt.missingStep != "" {
				for _, d := range []string{dirs.SnapHistory, dirs.SnapIdx, dirs.SnapAccessors} {
					files, err := filepath.Glob(filepath.Join(d, "*-rcache."+tt.missingStep+".*"))
					require.NoError(t, err)
					require.NotEmpty(t, files)
					for _, f := range files {
						require.NoError(t, dir.RemoveFile(f))
					}
				}
			}

			agg := dbstate.NewTest(dirs).StepSize(2).MustOpen(t.Context())
			db, err := temporal.New(mdbxtest.NewTestDB(t, dbcfg.ChainDB), agg, nil)
			require.NoError(t, err)
			defer db.Close()
			require.NoError(t, db.OpenStateSnapshots(t.Context()))
			tx, err := db.BeginTemporalRo(t.Context())
			require.NoError(t, err)
			defer tx.Rollback()

			end, err := rcacheHistoryEnd(tx)
			if tt.missingStep != "" && !tt.lateStart {
				require.ErrorContains(t, err, "gap", "in a gap HistorySeek returns a later receipt instead of failing")
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(13), end, "HistorySeek serves only txNums below the last write")
			_, _, err = tx.HistorySeek(kv.RCacheDomain, rawtemporaldb.ReceiptCacheKey, 1)
			require.Equal(t, tt.lateStart, err != nil, "before the first file HistorySeek fails instead of answering")
		})
	}
}

// buildRcacheFiles writes the receipt cache at every odd txNum below 15 and builds one file per
// step of 2 txNums.
func buildRcacheFiles(t *testing.T, dirs datadir.Dirs) {
	statecfg.EnableHistoricalRCache()
	ctx := t.Context()
	agg := dbstate.NewTest(dirs).StepSize(2).MustOpen(ctx)
	db, err := temporal.New(mdbxtest.NewTestDB(t, dbcfg.ChainDB), agg, nil)
	require.NoError(t, err)
	defer db.Close()

	rwTx, err := db.BeginTemporalRw(ctx)
	require.NoError(t, err)
	defer rwTx.Rollback()
	sd, err := execctx.NewSharedDomains(ctx, rwTx, log.New())
	require.NoError(t, err)
	defer sd.Close()
	for txNum := uint64(1); txNum < 15; txNum += 2 {
		require.NoError(t, sd.DomainPut(kv.RCacheDomain, rwTx, rawtemporaldb.ReceiptCacheKey, []byte{byte(txNum)}, txNum, nil))
	}
	// Files are only built for steps below the state progress.
	require.NoError(t, sd.DomainPut(kv.StorageDomain, rwTx, make([]byte, 52), []byte{1}, 17, nil))
	require.NoError(t, sd.Flush(ctx, rwTx))
	require.NoError(t, rwTx.Commit())
	require.NoError(t, db.BuildFiles2(ctx, 0, 8, execfinality.NewContext(^uint64(0), ^uint64(0), 0, false, rawdbv3.TxNums), false))
}

type fakeFile struct{ start, end uint64 }

func (f fakeFile) Fullpath() string         { return "" }
func (f fakeFile) StartRootNum() uint64     { return f.start }
func (f fakeFile) EndRootNum() uint64       { return f.end }
func (f fakeFile) Version() version.Version { return version.Version{} }

func TestContiguousRange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		files    kv.VisibleFiles
		from, to uint64
	}{
		{"no files", nil, 0, 0},
		{"contiguous", kv.VisibleFiles{fakeFile{0, 10}, fakeFile{10, 20}}, 0, 20},
		{"starts after 0", kv.VisibleFiles{fakeFile{5, 20}}, 5, 20},
		{"gap between files", kv.VisibleFiles{fakeFile{0, 5}, fakeFile{10, 20}}, 0, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			from, to := contiguousRange(tt.files)
			require.Equal(t, [2]uint64{tt.from, tt.to}, [2]uint64{from, to})
		})
	}
}
