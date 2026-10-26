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
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"golang.org/x/sync/semaphore"

	"github.com/erigontech/erigon/cmd/rpcdaemon/cli/httpcfg"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/dbservices"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/kvcache"
	kv2 "github.com/erigontech/erigon/db/kv/mdbx"
	"github.com/erigontech/erigon/db/kv/order"
	"github.com/erigontech/erigon/db/kv/temporal"
	"github.com/erigontech/erigon/db/rawdb"
	"github.com/erigontech/erigon/db/rawdb/rawtemporaldb"
	"github.com/erigontech/erigon/db/snapshotsync/blocksnapshots"
	"github.com/erigontech/erigon/db/snapshotsync/freezeblocks"
	"github.com/erigontech/erigon/db/snaptype"
	dbstate "github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/execution/chain"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/protocol/rules"
	"github.com/erigontech/erigon/execution/protocol/rules/ethash"
	"github.com/erigontech/erigon/execution/protocol/rules/merge"
	"github.com/erigontech/erigon/execution/stagedsync/stages"
	"github.com/erigontech/erigon/execution/state/genesiswrite"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node/gointerfaces/txpoolproto"
	"github.com/erigontech/erigon/rpc/rpchelper"
)

var logIndexIIs = []kv.InvertedIdx{kv.LogAddrIdx, kv.LogTopicIdx}

var snapshotOnlyNamespaces = []string{"eth", "erigon", "debug", "ots", "net", "web3"}

// snapshotOnlyMethods read only block segments and, for receipts and logs, the rcache domain and
// log index. Paths that reach other data, such as eth_feeHistory reward percentiles, fail on Restrict.
var snapshotOnlyMethods = []string{
	"eth_blockNumber", "eth_chainId", "eth_syncing", "eth_config",
	"eth_getBlockByNumber", "eth_getBlockByHash",
	"eth_getBlockTransactionCountByNumber", "eth_getBlockTransactionCountByHash",
	"eth_getTransactionByHash", "eth_getTransactionByBlockHashAndIndex", "eth_getTransactionByBlockNumberAndIndex",
	"eth_getRawTransactionByHash", "eth_getRawTransactionByBlockHashAndIndex", "eth_getRawTransactionByBlockNumberAndIndex",
	"eth_getUncleByBlockHashAndIndex", "eth_getUncleByBlockNumberAndIndex",
	"eth_getUncleCountByBlockHash", "eth_getUncleCountByBlockNumber",
	"eth_feeHistory", "eth_getTransactionReceipt", "eth_getBlockReceipts", "eth_getLogs",
	"erigon_blockNumber", "erigon_forks", "erigon_getHeaderByNumber", "erigon_getHeaderByHash",
	"erigon_getBlockByTimestamp", "erigon_getBlockReceiptsByBlockHash", "erigon_getLogsByHash",
	"erigon_getLogs",
	"debug_getRawBlock", "debug_getRawHeader", "debug_getRawTransaction", "debug_getRawReceipts",
	"ots_getApiLevel", "ots_getBlockDetails", "ots_getBlockDetailsByHash", "ots_getBlockTransactions",
	"net_version", "web3_clientVersion", "web3_sha3",
}

// ConfigureSnapshotOnly sets cfg up to serve the segments in snapshotDir, the snapshots directory of
// a datadir, on listen. chain is needed only when block 0 is the genesis of more than one known chain.
func ConfigureSnapshotOnly(cfg *httpcfg.HttpCfg, snapshotDir, listen, chain string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	if cfg.HttpPort, err = strconv.Atoi(port); err != nil {
		return fmt.Errorf("--listen: %w", err)
	}
	absSnapshotDir, err := filepath.Abs(snapshotDir)
	if err != nil {
		return err
	}
	// The lock a node takes and the receipts imported from era archives are kept in the datadir.
	dirs := datadir.Open(filepath.Dir(absSnapshotDir))
	if dirs.Snap != absSnapshotDir {
		return fmt.Errorf("--snapshot-dir %s is not the %s directory of a datadir", snapshotDir, datadir.SnapDir)
	}
	cfg.SnapshotOnly, cfg.Dirs, cfg.API = true, dirs, snapshotOnlyNamespaces
	cfg.HttpListenAddress, cfg.Snap.ChainName = host, chain
	return nil
}

// SnapshotOnlyServices serves block and receipt data straight from a datadir holding block
// segments and the rcache domain files, with no Erigon node and no state.
func SnapshotOnlyServices(ctx context.Context, cfg *httpcfg.HttpCfg, logger log.Logger) (
	db kv.TemporalRoDB, eth rpchelper.ApiBackend, txPool txpoolproto.TxpoolClient, mining txpoolproto.MiningClient,
	stateCache kvcache.Cache, blockReader dbservices.FullBlockReader, engine rules.Engine,
	ff *rpchelper.Filters, err error,
) {
	temporalDB, br, cc, err := openSnapshotOnlyDB(ctx, cfg, logger)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, nil, nil, err
	}
	engine = ethash.NewFaker()
	if cc.TerminalTotalDifficulty != nil {
		engine = merge.New(engine)
	}
	eth = &snapshotOnlyBackend{chainID: cc.ChainID.Uint64()}
	ff = rpchelper.New(ctx, cfg.RpcFiltersConfig, nil, nil, nil, func() {}, logger, nil)
	return temporalDB, eth, emptyTxPool{}, nil, kvcache.NewLatestBatchCache(), br, engine, ff, nil
}

// openSnapshotOnlyDB opens the datadir, records the last block to serve as its head and denies
// every read that would need state.
func openSnapshotOnlyDB(ctx context.Context, cfg *httpcfg.HttpCfg, logger log.Logger) (_ *temporal.DB, _ *freezeblocks.BlockReader, _ *chain.Config, err error) {
	var closers []func()
	defer func() {
		if err != nil {
			for _, closeFn := range slices.Backward(closers) {
				closeFn()
			}
		}
	}()

	_, lock, err := cfg.Dirs.MustFlock()
	if err != nil {
		return nil, nil, nil, err
	}
	closers = append(closers, func() { _ = lock.Close() })
	if err := dbstate.CheckSnapshotsCompatibility(cfg.Dirs); err != nil {
		return nil, nil, nil, err
	}
	stateDirs, removeStateDirs, err := snapshotOnlyStateDirs(cfg.Dirs, logger)
	if err != nil {
		return nil, nil, nil, err
	}
	closers = append(closers, removeStateDirs)

	// The block files are read before the chain is known, from --chain if at all; only the merge
	// limits depend on its name.
	snapCfg := cfg.Snap
	snapCfg.ProduceE2 = true // lets BuildMissedIndices build
	allSnapshots := blocksnapshots.NewRoSnapshots(snapCfg, cfg.Dirs.Snap, logger)
	closers = append(closers, allSnapshots.Close)
	allSnapshots.DownloadComplete()
	if err := allSnapshots.OpenFolder(); err != nil {
		return nil, nil, nil, err
	}
	if err := buildBlockIndexes(ctx, allSnapshots, cfg.Dirs, logger); err != nil {
		return nil, nil, nil, err
	}
	br := freezeblocks.NewBlockReader(allSnapshots)
	view := allSnapshots.View()
	genesis, err := br.HeaderFromView(view, 0)
	view.Close()
	if err != nil {
		return nil, nil, nil, err
	}
	if genesis == nil {
		return nil, nil, nil, errors.New("block segments do not hold block 0")
	}
	spec, err := snapshotOnlySpec(cfg.Snap.ChainName, genesis.Hash())
	if err != nil {
		return nil, nil, nil, err
	}
	if spec.Config.Aura != nil {
		return nil, nil, nil, fmt.Errorf("ss2rpc does not support AuRa chains such as %s", spec.Name)
	}

	// The datadir gets nothing but the block indexes built above. What RPC reads from the database
	// rather than from files - block 0, the chain config, the head - goes to a throwaway database on
	// every start.
	rawDB, err := kv2.New(dbcfg.ChainDB, logger).InMem("").RoTxsLimiter(semaphore.NewWeighted(int64(cfg.DBReadConcurrency))).Open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	closers = append(closers, rawDB.Close)
	if err := rawDB.Update(ctx, func(tx kv.RwTx) error { return writeGenesis(tx, spec, genesis) }); err != nil {
		return nil, nil, nil, err
	}

	statecfg.EnableHistoricalRCache()
	erigonDBSettings, err := dbstate.ResolveErigonDBSettings(stateDirs, logger, true)
	if err != nil {
		return nil, nil, nil, err
	}
	agg, err := dbstate.New(stateDirs).Logger(logger).WithErigonDBSettings(erigonDBSettings).Open(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create aggregator: %w", err)
	}
	closers = append(closers, agg.Close)
	temporalDB, err := temporal.New(rawDB, agg, allSnapshots)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := temporalDB.OpenStateSnapshots(ctx); err != nil {
		return nil, nil, nil, err
	}

	served, err := servedRange(ctx, temporalDB, br)
	if err != nil {
		return nil, nil, nil, err
	}
	if !served.receipts {
		logger.Warn("[rpc] no rcache files cover the block segments: receipt and log methods will fail")
	}
	if err := temporalDB.Update(ctx, func(tx kv.RwTx) error { return writeHead(tx, served.head) }); err != nil {
		return nil, nil, nil, err
	}
	// Without state files any other read would answer as if the state were empty.
	var readableIIs []kv.InvertedIdx
	if served.logIndex {
		readableIIs = logIndexIIs
	}
	temporalDB.Restrict([]kv.Domain{kv.RCacheDomain}, readableIIs)
	logger.Info("[rpc] serving snapshots only", "chain", spec.Name, "lastBlock", served.head.Number.Uint64(), "logIndex", served.logIndex)

	go func() {
		<-ctx.Done()
		removeStateDirs()
		_ = lock.Close()
	}()
	return temporalDB, br, spec.Config, nil
}

// buildBlockIndexes builds the indexes the block segments lack beside them, as a node does: peers
// share only the segments. The indexes do not depend on the chain, which is known only once block 0
// can be read through them.
func buildBlockIndexes(ctx context.Context, snapshots *blocksnapshots.RoSnapshots, dirs datadir.Dirs, logger log.Logger) error {
	if _, err := snaptype.LoadSalt(dirs.Snap, true, logger); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "ss2rpc-indexes-")
	if err != nil {
		return err
	}
	defer func() { _ = dir.RemoveAll(tmp) }()
	dirs.Tmp = tmp
	return snapshots.BuildMissedIndices(ctx, "[rpc]", nil, dirs, nil, logger)
}

// snapshotOnlyStateDirs is where the receipt cache and log indexes are read from: the ones imported
// from era1, else a node's own, else an empty directory removed on close, which serves blocks only.
// A directory counts once its state salt exists, as the files cannot be read without it, and then
// needs its settings too: a guessed step size would misplace every file.
func snapshotOnlyStateDirs(dirs datadir.Dirs, logger log.Logger) (datadir.Dirs, func(), error) {
	era := dirs.EraReceipts()
	for _, d := range []*datadir.Dirs{&era, &dirs} {
		salted, err := dir.FileExist(filepath.Join(d.Snap, "salt-state.txt"))
		if err != nil {
			return datadir.Dirs{}, nil, err
		}
		if !salted {
			continue
		}
		settings := filepath.Join(d.Snap, dbstate.ERIGONDB_SETTINGS_FILE)
		if ok, err := dir.FileExist(settings); err != nil || !ok {
			return datadir.Dirs{}, nil, errors.Join(err, fmt.Errorf("%s is missing: the state files' step size is unknown", settings))
		}
		return *d, func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "snapshot-only-state-")
	if err != nil {
		return datadir.Dirs{}, nil, err
	}
	empty := datadir.New(tmp)
	if _, err := dbstate.GetStateIndicesSalt(empty, true, logger); err != nil {
		_ = dir.RemoveAll(tmp)
		return datadir.Dirs{}, nil, err
	}
	return empty, func() { _ = dir.RemoveAll(tmp) }, nil
}

// snapshotOnlySpec returns the chain the block files belong to: chainName checked against their
// block 0, or the one chain whose genesis that block is.
func snapshotOnlySpec(chainName string, genesis common.Hash) (chainspec.Spec, error) {
	if chainName != "" {
		spec, err := chainspec.ChainSpecByName(chainName)
		if err != nil {
			return chainspec.Spec{}, err
		}
		if spec.GenesisHash != genesis {
			return chainspec.Spec{}, fmt.Errorf("block segments start at %x, not at the %s genesis %x", genesis, chainName, spec.GenesisHash)
		}
		return spec, nil
	}
	switch specs := chainspec.ChainSpecsByGenesisHash(genesis); len(specs) {
	case 0:
		return chainspec.Spec{}, fmt.Errorf("block 0 %x is not the genesis of a known chain", genesis)
	case 1:
		return specs[0], nil
	default:
		return chainspec.Spec{}, fmt.Errorf("block 0 %x is the genesis of %d known chains: pass --chain", genesis, len(specs))
	}
}

// writeGenesis writes block 0, taken from the segments so that no genesis state has to be computed.
// RPC reads block 0 from the database only.
func writeGenesis(tx kv.RwTx, spec chainspec.Spec, header *types.Header) error {
	_, withdrawals := genesiswrite.GenesisWithoutStateToBlock(spec.Genesis)
	return genesiswrite.WriteGenesisBesideState(types.NewBlock(header, nil, nil, nil, withdrawals, nil), tx, spec.Genesis)
}

type servedBlocks struct {
	head     *types.Header // the last block served
	receipts bool          // receipts are served up to head
	logIndex bool          // the log index is served up to head
}

// servedRange works out the last block to serve: the last frozen block, lowered to the last block
// rcache fully covers.
func servedRange(ctx context.Context, db *temporal.DB, br *freezeblocks.BlockReader) (served servedBlocks, err error) {
	tx, err := db.BeginTemporalRo(ctx)
	if err != nil {
		return served, err
	}
	defer tx.Rollback()

	lastBlock := br.FrozenBlocks()
	if lastBlock == 0 {
		return served, errors.New("no indexed block segments found")
	}
	txNums := br.TxnumReader()
	lastBlockEnd, err := txNums.Max(ctx, tx, lastBlock)
	if err != nil {
		return served, err
	}
	rcacheEnd, err := rcacheHistoryEnd(tx)
	if err != nil {
		return served, err
	}
	frontier, receipts, err := receiptsFrontier(lastBlock, lastBlockEnd, rcacheEnd, func(txNum uint64) (uint64, error) {
		block, ok, err := txNums.FindBlockNum(ctx, tx, txNum)
		if err == nil && !ok {
			err = fmt.Errorf("no block holds txNum %d", txNum)
		}
		return block, err
	})
	if err != nil {
		return served, err
	}
	frontierEnd, err := txNums.Max(ctx, tx, frontier)
	if err != nil {
		return served, err
	}
	head, err := br.HeaderByNumber(ctx, tx, frontier)
	if err != nil {
		return served, err
	}
	if head == nil {
		return served, fmt.Errorf("header %d not found in block segments", frontier)
	}
	aggTx := tx.AggTx().(*dbstate.AggregatorRoTx)
	logIndex := true
	for _, ii := range logIndexIIs {
		// An index read finds nothing, rather than fails, before, between and after the files.
		from, to := contiguousRange(aggTx.DbgII(ii).Files())
		logIndex = logIndex && from == 0 && to > frontierEnd
	}
	return servedBlocks{head: head, receipts: receipts, logIndex: logIndex}, nil
}

// writeHead records head as the execution progress and the head header, which is what RPC
// resolves "latest" and the current header against.
func writeHead(tx kv.RwTx, head *types.Header) error {
	if err := rawdb.WriteHeader(tx, head); err != nil {
		return err
	}
	if err := rawdb.WriteHeadHeaderHash(tx, head.Hash()); err != nil {
		return err
	}
	return stages.SaveStageProgress(tx, stages.Execution, head.Number.Uint64())
}

// contiguousRange returns the txNums [from, to) the files hold without a gap, from the first file on.
func contiguousRange(files kv.VisibleFiles) (from, to uint64) {
	if len(files) == 0 {
		return 0, 0
	}
	from, to = files[0].StartRootNum(), files[0].EndRootNum()
	for _, f := range files[1:] {
		if f.StartRootNum() != to {
			break
		}
		to = f.EndRootNum()
	}
	return from, to
}

// rcacheHistoryEnd returns the txNum below which receipts are served from rcache history.
// Receipts are read with HistorySeek(txNum+1), which needs a later write of the key, so the bound
// is the last recorded write rather than the end of the files: the domain holds a single key and
// its file usually spans the whole chain.
func rcacheHistoryEnd(tx kv.TemporalTx) (uint64, error) {
	files := tx.AggTx().(*dbstate.AggregatorRoTx).DbgHistoryII(kv.RCacheDomain).Files()
	_, end := contiguousRange(files)
	if len(files) > 0 && end != files[len(files)-1].EndRootNum() {
		// Before the first file HistorySeek fails, but in a gap it returns another transaction's
		// receipt. Receipt reads do not stop at the served head.
		return 0, fmt.Errorf("receipt cache files have a gap at txNum %d", end)
	}
	if end == 0 {
		return 0, nil
	}
	it, err := tx.IndexRange(kv.RCacheHistoryIdx, rawtemporaldb.ReceiptCacheKey, int(end-1), 0, order.Desc, 1)
	if err != nil {
		return 0, err
	}
	defer it.Close()
	if !it.HasNext() {
		return 0, nil
	}
	return it.Next()
}

// receiptsFrontier returns the last block whose receipts all lie below rcacheEnd, capped at
// lastBlock. A block's last txNum is its block-end system tx, which has no receipt, so the last
// covered block is the one before the block holding rcacheEnd+1. receipts is false when no block
// is covered; the frontier then stays at lastBlock and receipt methods fail.
func receiptsFrontier(lastBlock, lastBlockEnd, rcacheEnd uint64, blockOf func(txNum uint64) (uint64, error)) (frontier uint64, receipts bool, err error) {
	if rcacheEnd >= lastBlockEnd {
		return lastBlock, true, nil
	}
	firstUncovered, err := blockOf(rcacheEnd + 1)
	if err != nil {
		return 0, false, err
	}
	if firstUncovered == 0 {
		return lastBlock, false, nil
	}
	return firstUncovered - 1, true, nil
}
