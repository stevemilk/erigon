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
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/era/eratest"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx"
	"github.com/erigontech/erigon/db/kv/order"
	"github.com/erigontech/erigon/db/kv/stream"
	"github.com/erigontech/erigon/db/rawdb"
	dbstate "github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/execution/types"
)

// TestImportEraReceiptsRejectsWrongRoot swaps two blocks' receipts. The archive's own checks do not
// look at receipts, so only the receipts root in the header can catch it.
func TestImportEraReceiptsRejectsWrongRoot(t *testing.T) {
	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0)
	path := chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	eratest.Rewrite(t, path, func(blocks []era.Block) {
		blocks[100].ReceiptsRLP, blocks[101].ReceiptsRLP = blocks[101].ReceiptsRLP, blocks[100].ReceiptsRLP
	})

	dirs := newEraReceiptsDatadir(t)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.ErrorContains(t, importEraReceipts(t, eraDir, dirs), "receipts root")
}

// TestImportEraReceiptsResumes imports one file, then the archive grown by a second. The second run
// has to carry on from the receipts already written, not rewrite or skip any.
func TestImportEraReceiptsResumes(t *testing.T) {
	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0)
	chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	dirs := newEraReceiptsDatadir(t)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.NoError(t, importEraReceipts(t, eraDir, dirs))

	chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.NoError(t, importEraReceipts(t, eraDir, dirs))

	requireReceiptsImported(t, dirs, 2*era.MaxBlocksPerFile)
}

// newEraReceiptsDatadir uses a step that does not divide the txNums of the test chains, so the last
// step is left unfinished and only the ones before it become files.
func newEraReceiptsDatadir(t *testing.T) datadir.Dirs {
	t.Helper()
	dirs := datadir.New(t.TempDir())
	layer := dirs.SnapshotLayer()
	require.NoError(t, os.MkdirAll(layer.Snap, 0o755))
	settings := []byte("step_size = 5000\nsteps_in_frozen_file = 4\nreferences_in_commitment_branches = false\n")
	require.NoError(t, os.WriteFile(filepath.Join(layer.Snap, "erigondb.toml"), settings, 0o644))
	return dirs
}

func importEraReceipts(t *testing.T, eraDir string, dirs datadir.Dirs) error {
	t.Helper()
	cfg, _ := mainnetSpec(t)
	return ImportEraReceipts(t.Context(), eraDir, dirs, cfg, log.New())
}

// requireReceiptsImported reads every receipt back through the paths RPC uses, and checks that
// only the receipt and log files were built.
func requireReceiptsImported(t *testing.T, dirs datadir.Dirs, blocks uint64) {
	t.Helper()
	ctx := t.Context()
	cfg, _ := mainnetSpec(t)
	db, br, closeDB, err := openEraReceiptsDB(ctx, dirs, cfg, log.New())
	require.NoError(t, err)
	defer closeDB()
	tx, err := db.BeginTemporalRo(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	txNums := br.TxnumReader()
	// Block 0 is always read from the database, which the import leaves without blocks.
	for num := uint64(1); num < blocks; num++ {
		hash, ok, err := br.CanonicalHash(ctx, tx, num)
		require.NoError(t, err)
		require.True(t, ok)
		block, senders, err := br.BlockWithSenders(ctx, tx, hash, num)
		require.NoError(t, err)
		minTxNum, err := txNums.Min(ctx, tx, num)
		require.NoError(t, err)

		for i, txn := range block.Transactions() {
			txNum := minTxNum + 1 + uint64(i)
			receipt, ok, err := rawdb.ReadReceiptCacheV2(tx, rawdb.RCacheV2Query{TxNum: txNum, BlockNum: num, BlockHash: hash, TxnHash: txn.Hash()})
			require.NoError(t, err)
			require.Truef(t, ok, "block %d txn %d has no receipt", num, i)
			require.Equal(t, types.ReceiptStatusSuccessful, receipt.Status)
			require.Equal(t, uint64(i+1)*21000, receipt.CumulativeGasUsed)
			require.Equal(t, uint64(21000), receipt.GasUsed)
			require.Equal(t, uint(i), receipt.TransactionIndex)
			require.Equal(t, uint32(i), receipt.FirstLogIndexWithinBlock)
			require.Len(t, receipt.Logs, 1)
			require.Equal(t, eratest.LogAddress(num, i), receipt.Logs[0].Address)
			require.Equal(t, []common.Hash{eratest.LogTopic(num, i)}, receipt.Logs[0].Topics)
			if txn.GetTo() == nil {
				require.Equal(t, types.CreateAddress(senders[i], txn.GetNonce()), receipt.ContractAddress)
			} else {
				require.Equal(t, common.Address{}, receipt.ContractAddress)
			}

			requireIndexed(t, tx, kv.LogAddrIdx, addrKey(eratest.LogAddress(num, i)), txNum)
			requireIndexed(t, tx, kv.LogTopicIdx, topicKey(eratest.LogTopic(num, i)), txNum)
		}
	}

	layer := dirs.SnapshotLayer()
	require.NotEmpty(t, mustGlob(t, layer.SnapHistory, "*-rcache.*.v"))
	require.NotEmpty(t, mustGlob(t, layer.SnapIdx, "*-logaddrs.*.ef"))
	require.NotEmpty(t, mustGlob(t, layer.SnapIdx, "*-logtopics.*.ef"))
	for _, d := range []string{dirs.SnapDomain, dirs.SnapHistory, dirs.SnapIdx, dirs.SnapAccessors} {
		require.Emptyf(t, mustGlob(t, d, "*"), "the node's own state directories stay untouched: %s", d)
	}
	require.NoFileExists(t, filepath.Join(dirs.Snap, "salt-state.txt"), "the node's state salt stays untouched")
	for _, d := range []string{layer.SnapDomain, layer.SnapHistory, layer.SnapIdx} {
		for _, name := range []string{"accounts", "storage", "code", "commitment", "receipt.", "tracesfrom", "tracesto"} {
			require.Emptyf(t, mustGlob(t, d, "*-"+name+"*"), "no %s files: nothing executed", name)
		}
	}
}

func requireIndexed(t *testing.T, tx kv.TemporalTx, idx kv.InvertedIdx, key []byte, txNum uint64) {
	t.Helper()
	it, err := tx.IndexRange(idx, key, int(txNum), int(txNum)+1, order.Asc, -1)
	require.NoError(t, err)
	got, err := stream.ToArrayU64(it)
	require.NoError(t, err)
	require.Equalf(t, []uint64{txNum}, got, "%s does not point %x at txNum %d", idx, key, txNum)
}

func addrKey(a common.Address) []byte { return a[:] }
func topicKey(h common.Hash) []byte   { return h[:] }

// TestImportEraReceiptsMergesAndPrunes: with no state domain open, the files still have to merge
// like a node's, and what they hold has to leave the database.
func TestImportEraReceiptsMergesAndPrunes(t *testing.T) {
	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0)
	for range 2 {
		chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	}
	dirs := newEraReceiptsDatadir(t)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.NoError(t, importEraReceipts(t, eraDir, dirs))
	requireReceiptsImported(t, dirs, 2*era.MaxBlocksPerFile)

	layer := dirs.SnapshotLayer()
	for _, pattern := range []string{"*-rcache.0-4.v", "*-logaddrs.0-4.ef", "*-logtopics.0-4.ef"} {
		dir := layer.SnapIdx
		if filepath.Ext(pattern) == ".v" {
			dir = layer.SnapHistory
		}
		require.NotEmptyf(t, mustGlob(t, dir, pattern), "%s: the first frozen file of 4 steps", pattern)
	}

	db := mdbx.New(dbcfg.ChainDB, log.New()).Path(layer.Chaindata).MustOpen()
	defer db.Close()
	require.NoError(t, db.View(t.Context(), func(tx kv.Tx) error {
		first, err := kv.FirstKey(tx, kv.TblRCacheHistoryKeys)
		require.NoError(t, err)
		require.NotEmpty(t, first, "the unfinished last step stays in the database")
		filesEnd := uint64(2*era.MaxBlocksPerFile*4) / 5000 * 5000
		require.GreaterOrEqual(t, binary.BigEndian.Uint64(first), filesEnd, "receipts the files hold are pruned from the database")
		return nil
	}))

	// A reader opens every domain. Pruning must not have marked the ones that were off as pruned
	// past their (absent) files.
	settings, err := dbstate.ResolveErigonDBSettings(layer, log.New(), true)
	require.NoError(t, err)
	agg, err := dbstate.New(layer).Logger(log.New()).WithErigonDBSettings(settings).Open(t.Context())
	require.NoError(t, err)
	defer agg.Close()
	require.NoError(t, agg.OpenFolder(db))
}

// TestImportEraReceiptsStopsAtArchiveEnd: the segments can reach past the archive, as a node's do.
// Receipts are imported as far as the archive goes, and the rest is left for a later run.
func TestImportEraReceiptsStopsAtArchiveEnd(t *testing.T) {
	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0)
	chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	second := chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	dirs := newEraReceiptsDatadir(t)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.NoError(t, dir.RemoveFile(second))

	require.NoError(t, importEraReceipts(t, eraDir, dirs))
	requireReceiptsImported(t, dirs, era.MaxBlocksPerFile)
}

// TestImportEraReceiptsKeepsPostStateRoots: receipts from before Byzantium carry a post-state root
// and no status; the import keeps the root as the archive has it.
func TestImportEraReceiptsKeepsPostStateRoots(t *testing.T) {
	eraDir := t.TempDir()
	chain := eratest.NewChain(t, 0).UsePostStateRoots()
	chain.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	dirs := newEraReceiptsDatadir(t)
	require.NoError(t, importEra(t, eraDir, dirs, chain))
	require.NoError(t, importEraReceipts(t, eraDir, dirs))

	cfg, _ := mainnetSpec(t)
	db, br, closeDB, err := openEraReceiptsDB(t.Context(), dirs, cfg, log.New())
	require.NoError(t, err)
	defer closeDB()
	tx, err := db.BeginTemporalRo(t.Context())
	require.NoError(t, err)
	defer tx.Rollback()
	const num = 100
	hash, _, err := br.CanonicalHash(t.Context(), tx, num)
	require.NoError(t, err)
	block, _, err := br.BlockWithSenders(t.Context(), tx, hash, num)
	require.NoError(t, err)
	minTxNum, err := br.TxnumReader().Min(t.Context(), tx, num)
	require.NoError(t, err)
	for i, txn := range block.Transactions() {
		receipt, ok, err := rawdb.ReadReceiptCacheV2(tx, rawdb.RCacheV2Query{TxNum: minTxNum + 1 + uint64(i), BlockNum: num, BlockHash: hash, TxnHash: txn.Hash()})
		require.NoError(t, err)
		require.True(t, ok)
		root := eratest.PostStateRoot(num, i)
		require.Equal(t, root[:], receipt.PostState)
	}
}
