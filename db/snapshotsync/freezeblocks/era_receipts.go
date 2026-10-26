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
	"context"
	"fmt"
	"time"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/hexutil"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx"
	"github.com/erigontech/erigon/db/kv/temporal"
	"github.com/erigontech/erigon/db/rawdb/rawtemporaldb"
	"github.com/erigontech/erigon/db/snapshotsync/blocksnapshots"
	dbstate "github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/db/state/execctx"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/execfinality"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node/ethconfig"
)

// eraReceiptsBatch is how many blocks are written in one transaction.
const eraReceiptsBatch = 20_000

// ImportEraReceipts writes the archive's receipts into the rcache domain and the log indexes, for
// the blocks the block segments hold, and builds their files. Nothing is executed: each block's
// receipts are checked against the receipts root of its header instead.
func ImportEraReceipts(ctx context.Context, eraDir string, dirs datadir.Dirs, chainConfig *chain.Config, logger log.Logger) error {
	archive, err := era.OpenArchive(eraDir, chainConfig.ChainName)
	if err != nil {
		return err
	}
	defer archive.Close()
	db, br, closeDB, err := openEraReceiptsDB(ctx, dirs, chainConfig, logger)
	if err != nil {
		return err
	}
	defer closeDB()

	archiveEnd, err := archive.End()
	if err != nil {
		return err
	}
	end := min(br.FrozenBlocks()+1, archiveEnd)
	from, err := firstBlockWithoutReceipts(ctx, db, br)
	if err != nil {
		return err
	}
	logger.Info("[era-import] receipts", "from", from, "to", end)

	view := br.Snapshots().(*blocksnapshots.RoSnapshots).View()
	defer view.Close()
	for from < end {
		to := min(end, from+eraReceiptsBatch)
		if err := writeEraReceipts(ctx, db, br, view, archive, chainConfig, from, to, logger); err != nil {
			return err
		}
		if err := buildEraReceiptsFiles(ctx, db, br); err != nil {
			return err
		}
		logger.Info("[era-import] receipts done", "to", to)
		from = to
	}
	return nil
}

// openEraReceiptsDB opens the era receipts directories with only the receipt cache and the log
// indexes switched on, so that nothing else is opened or built. Blocks are read from the datadir's
// own segments, which the node shares.
func openEraReceiptsDB(ctx context.Context, dirs datadir.Dirs, chainConfig *chain.Config, logger log.Logger) (*temporal.DB, *BlockReader, func(), error) {
	eraDirs := dirs.EraReceipts()
	dir.MustExist(eraDirs.Chaindata, eraDirs.SnapDomain, eraDirs.SnapHistory, eraDirs.SnapIdx, eraDirs.SnapAccessors)
	if _, err := dbstate.GetStateIndicesSalt(eraDirs, true, logger); err != nil {
		return nil, nil, nil, err
	}
	settings, err := dbstate.ResolveErigonDBSettings(eraDirs, logger, true)
	if err != nil {
		return nil, nil, nil, err
	}

	opened := false
	snaps := blocksnapshots.NewRoSnapshots(ethconfig.BlocksFreezing{ChainName: chainConfig.ChainName}, dirs.Snap, logger)
	defer func() {
		if !opened {
			snaps.Close()
		}
	}()
	err = snaps.OpenFolder()
	if err != nil {
		return nil, nil, nil, err
	}
	rawDB, err := mdbx.New(dbcfg.ChainDB, logger).Path(eraDirs.Chaindata).Open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if !opened {
			rawDB.Close()
		}
	}()
	statecfg.EnableHistoricalRCache()
	agg, err := dbstate.New(eraDirs).Logger(logger).WithErigonDBSettings(settings).Open(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() {
		if !opened {
			agg.Close()
		}
	}()
	for d := range kv.DomainLen {
		if d != kv.RCacheDomain {
			agg.DisableDomain(d)
		}
	}
	agg.DisableIdx(kv.TracesFromIdx)
	agg.DisableIdx(kv.TracesToIdx)
	err = agg.OpenFolder(rawDB)
	if err != nil {
		return nil, nil, nil, err
	}
	db, err := temporal.New(rawDB, agg, snaps)
	if err != nil {
		return nil, nil, nil, err
	}
	opened = true
	return db, NewBlockReader(snaps), func() { db.Close(); snaps.Close() }, nil
}

// firstBlockWithoutReceipts is where an import carries on. Every block is written whole, ending
// with its block-end entry, so the receipt cache's progress is the last txNum of a block.
func firstBlockWithoutReceipts(ctx context.Context, db *temporal.DB, br *BlockReader) (uint64, error) {
	tx, err := db.BeginTemporalRo(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	progress := tx.Debug().DomainProgress(kv.RCacheDomain)
	if progress == 0 {
		return 0, nil
	}
	block, ok, err := br.TxnumReader().FindBlockNum(ctx, tx, progress)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("era import: the receipt cache reaches txNum %d, past the block segments", progress)
	}
	return block + 1, nil
}

func writeEraReceipts(ctx context.Context, db *temporal.DB, br *BlockReader, view *blocksnapshots.View, archive *era.Archive, chainConfig *chain.Config, from, to uint64, logger log.Logger) error {
	tx, err := db.BeginTemporalRw(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	doms, err := execctx.NewSharedDomains(ctx, tx, logger)
	if err != nil {
		return err
	}
	defer doms.Close()
	putter := doms.AsPutDel(tx)
	var w rawtemporaldb.ReceiptWriter

	txNum, err := br.TxnumReader().Min(ctx, tx, from)
	if err != nil {
		return err
	}
	for num := from; num < to; num++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		segmentHeader, err := br.HeaderFromView(view, num)
		if err != nil {
			return err
		}
		if segmentHeader == nil {
			return fmt.Errorf("era import: block %d is not in the block segments", num)
		}
		f, err := archive.File(num)
		if err != nil {
			return err
		}
		receipts, err := eraReceipts(f, chainConfig, num, segmentHeader.Hash())
		if err != nil {
			return err
		}

		// The block-begin and block-end system transactions have no receipt, but get an entry all
		// the same, as execution writes them.
		if err := w.Append(putter, nil, txNum); err != nil {
			return err
		}
		for _, receipt := range receipts {
			txNum++
			if err := w.Append(putter, receipt, txNum); err != nil {
				return err
			}
			for _, l := range receipt.Logs {
				if err := doms.IndexAdd(kv.LogAddrIdx, l.Address[:], txNum); err != nil {
					return err
				}
				for _, topic := range l.Topics {
					if err := doms.IndexAdd(kv.LogTopicIdx, topic[:], txNum); err != nil {
						return err
					}
				}
			}
		}
		txNum++
		if err := w.Append(putter, nil, txNum); err != nil {
			return err
		}
		txNum++
	}
	if lastTxNum, err := br.TxnumReader().Max(ctx, tx, to-1); err != nil {
		return err
	} else if lastTxNum+1 != txNum {
		return fmt.Errorf("era import: receipts of blocks [%d,%d) end at txNum %d, the block segments at %d", from, to, txNum-1, lastTxNum)
	}
	if err := doms.Flush(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// buildEraReceiptsFiles turns every step the receipts have filled into files, and drops what the
// files now hold from the database.
func buildEraReceiptsFiles(ctx context.Context, db *temporal.DB, br *BlockReader) error {
	agg := db.Agg().(*dbstate.Aggregator)
	var fromStep, toStep kv.Step
	if err := db.ViewTemporal(ctx, func(tx kv.TemporalTx) error {
		aggTx := tx.AggTx().(*dbstate.AggregatorRoTx)
		fromStep = kv.Step(aggTx.DbgDomain(kv.RCacheDomain).FirstStepNotInFiles())
		toStep = kv.Step((tx.Debug().DomainProgress(kv.RCacheDomain) + 1) / agg.StepSize())
		return nil
	}); err != nil {
		return err
	}
	if toStep <= fromStep {
		return nil
	}
	finality := execfinality.NewContext(^uint64(0), ^uint64(0), 0, false, br.TxnumReader())
	if err := agg.BuildFiles2(ctx, db, fromStep, toStep, finality, true); err != nil {
		return err
	}
	<-agg.WaitForBuildAndMerge(ctx)
	return db.UpdateTemporal(ctx, func(tx kv.TemporalRwTx) error {
		_, err := tx.PruneSmallBatches(ctx, time.Hour)
		return err
	})
}

// eraReceipts reads block num's receipts and completes them with what the consensus encoding leaves
// out. The block's hash in the segments pins the file to the same chain, and the receipts root in
// its header pins the receipts.
func eraReceipts(f *era.Era1, chainConfig *chain.Config, num uint64, hash common.Hash) (types.Receipts, error) {
	header, archived, err := f.Header(num)
	if err != nil {
		return nil, err
	}
	if archived != hash {
		return nil, fmt.Errorf("era import: block %d is %x in the archive but %x in the segments", num, archived, hash)
	}
	body, err := f.Body(num)
	if err != nil {
		return nil, err
	}
	receipts, err := f.Receipts(num)
	if err != nil {
		return nil, err
	}
	if len(receipts) != len(body.Transactions) {
		return nil, fmt.Errorf("era import: block %d has %d transactions but %d receipts", num, len(body.Transactions), len(receipts))
	}
	if root := types.DeriveSha(receipts); root != header.ReceiptHash {
		return nil, fmt.Errorf("era import: block %d receipts root is %x, the header has %x", num, root, header.ReceiptHash)
	}

	signer := types.MakeSigner(chainConfig, num, header.Time)
	var logIndex uint32
	var cumulativeGasUsed uint64
	for i, receipt := range receipts {
		receipt.TransactionIndex = uint(i)
		receipt.GasUsed = receipt.CumulativeGasUsed - cumulativeGasUsed
		cumulativeGasUsed = receipt.CumulativeGasUsed
		receipt.FirstLogIndexWithinBlock = logIndex
		for _, l := range receipt.Logs {
			l.Index = hexutil.Uint(logIndex)
			logIndex++
		}

		txn, err := types.DecodeTransaction(body.Transactions[i])
		if err != nil {
			return nil, fmt.Errorf("era import: block %d txn %d: %w", num, i, err)
		}
		if txn.GetTo() == nil {
			sender, err := txn.Sender(*signer)
			if err != nil {
				return nil, fmt.Errorf("era import: block %d txn %d sender: %w", num, i, err)
			}
			receipt.ContractAddress = types.CreateAddress(sender.Value(), txn.GetNonce())
		}
	}
	return receipts, nil
}
