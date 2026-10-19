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
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/estimate"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/snapcfg"
	"github.com/erigontech/erigon/db/snapshotsync/blocksnapshots"
	"github.com/erigontech/erigon/db/snaptype"
	"github.com/erigontech/erigon/db/snaptype2"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node/ethconfig"
)

// ImportEra builds epoch block segments from a run of archive files, continuing the segments already
// built. genesis anchors the run to a real chain: without it the files only have to be consistent
// with each other, which a fabricated chain also manages.
func ImportEra(ctx context.Context, eraDir string, dirs datadir.Dirs, chainConfig *chain.Config, snCfg *snapcfg.Cfg, genesis common.Hash, workers int, logger log.Logger) error {
	if !snaptype2.RegimeFor(chainConfig) {
		return fmt.Errorf("era import: %s does not use epoch segments", chainConfig.ChainName)
	}
	if genesis == (common.Hash{}) {
		return errors.New("era import: no genesis hash to anchor the archive on")
	}
	decimal, err := HasDecimalBlockSegments(dirs, chainConfig)
	if err != nil {
		return err
	}
	if decimal {
		return fmt.Errorf("era import: %s holds decimal block segments; start the node once to convert them", dirs.Snap)
	}
	// A datadir that has not downloaded snapshots yet has no salt file, and indexes need one.
	if _, err = snaptype.LoadSalt(dirs.Snap, true, logger); err != nil {
		return err
	}
	archive, err := era.OpenArchive(eraDir, chainConfig.ChainName)
	if err != nil {
		return err
	}
	defer archive.Close()
	end, err := archive.End()
	if err != nil {
		return err
	}
	from, firstTxNum, parent, err := builtTip(dirs, chainConfig.ChainName, logger)
	if err != nil {
		return err
	}
	// The archive may start earlier, but a gap means the blocks in between exist nowhere.
	if archive.From() > from {
		return fmt.Errorf("era import: the segments end at block %d but the archive starts at %d", from, archive.From())
	}
	plan, tailFrom, err := planEraImport(dirs.Snap, from, end, snCfg)
	if err != nil {
		return err
	}
	if len(plan) == 0 && from == 0 {
		return fmt.Errorf("era import: blocks up to %d are not enough to fill one segment", end)
	}
	logger.Info("[era-import] planned", "from", from, "end", end, "segments", len(plan), "tail-from", tailFrom)

	seed := func(context.Context) uint64 { return firstTxNum }
	src := &eraChain{archive: archive, parent: parent, next: from, genesis: genesis, logger: logger}
	for _, r := range plan {
		from, to := r[0], r[1]
		dump := func(t snaptype.Type, d dumpFunc) (uint64, error) {
			return dumpRange(ctx, t.FileInfo(dirs.Snap, true, from, to), d, seed, nil, chainConfig, dirs.Tmp, workers, log.LvlInfo, logger, nil)
		}
		if _, err = dump(snaptype2.Headers, src.dumpHeaders); err != nil {
			return err
		}
		if firstTxNum, err = dump(snaptype2.Bodies, src.dumpBodies); err != nil {
			return err
		}
		if _, err = dump(snaptype2.Transactions, src.dumpTxs); err != nil {
			return err
		}
		logger.Info("[era-import] segment done", "from", from, "to", to)
	}
	return nil
}

// builtTip is where an import carries on: the end of the last segment already built that also ends
// an archive file, the txNum that block starts at, and the hash of the block before it. A file is
// verified from its first block, so a run never resumes inside one.
func builtTip(dirs datadir.Dirs, chainName string, logger log.Logger) (block, txNum uint64, parent common.Hash, err error) {
	snaps := blocksnapshots.NewRoSnapshots(ethconfig.BlocksFreezing{ChainName: chainName}, dirs.Snap, logger)
	defer snaps.Close()
	if err := snaps.OpenFolder(); err != nil {
		return 0, 0, common.Hash{}, err
	}
	view := snaps.View()
	defer view.Close()
	bodies := view.Bodies()
	if len(bodies) > 0 && bodies[0].From() != 0 {
		return 0, 0, common.Hash{}, fmt.Errorf("era import: the block segments start at %d, not 0", bodies[0].From())
	}
	for _, seg := range slices.Backward(bodies) {
		if seg.To()%era.MaxBlocksPerFile != 0 {
			continue
		}
		last := seg.To() - 1
		body, _, err := BodyForStorageFromSnapshot(last, seg, nil)
		if err != nil {
			return 0, 0, common.Hash{}, err
		}
		header, err := NewBlockReader(snaps).HeaderFromView(view, last)
		if err != nil {
			return 0, 0, common.Hash{}, err
		}
		if body == nil || header == nil {
			return 0, 0, common.Hash{}, fmt.Errorf("era import: block %d is missing from its segments", last)
		}
		return seg.To(), body.BaseTxnID.LastSystemTx(body.TxCount) + 1, header.Hash(), nil
	}
	return 0, 0, common.Hash{}, nil
}

// planEraImport tiles [from,end) and refuses a plan that another segment covers part of a range of,
// such as one left by an interrupted run over an archive that has since grown and tiles differently.
func planEraImport(snapDir string, from, end uint64, snCfg *snapcfg.Cfg) (plan [][2]uint64, tailFrom uint64, err error) {
	plan, tailFrom = planEpochSegments(from, end, snCfg)
	paths, err := dir.ListFiles(snapDir)
	if err != nil {
		return nil, 0, err
	}
	for _, p := range paths {
		info, _, ok := snaptype.ParseFileName("", filepath.Base(p))
		if !ok || !info.Epoch || info.Ext != ".seg" {
			continue
		}
		for _, r := range plan {
			if info.From < r[1] && r[0] < info.To && (info.From != r[0] || info.To != r[1]) {
				return nil, 0, fmt.Errorf("era import: %s already covers part of [%d,%d); remove it and import again", info.Name(), r[0], r[1])
			}
		}
	}
	return plan, tailFrom, nil
}

// eraChain reads an archive in block order and verifies each file, the first time it is reached, as
// a continuation of the chain before it.
type eraChain struct {
	archive *era.Archive
	parent  common.Hash // hash of block next-1
	next    uint64      // first block not verified yet
	genesis common.Hash
	logger  log.Logger
}

func (c *eraChain) file(num uint64) (*era.Era1, error) {
	f, err := c.archive.File(num)
	if err != nil || f.Start() < c.next {
		return f, err
	}
	if f.Start() != c.next {
		return nil, fmt.Errorf("era import: %d starts the next file, but the chain so far ends at %d", f.Start(), c.next)
	}
	if c.parent, err = era.Verify(f, c.parent, c.genesis); err != nil {
		return nil, err
	}
	c.next = f.Last() + 1
	c.logger.Info("[era-import] verified", "blocks", fmt.Sprintf("%d-%d", f.Start(), f.Last()))
	return f, nil
}

func (c *eraChain) dumpHeaders(ctx context.Context, _ kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, _ int, _ log.Lvl, _ log.Logger) (uint64, error) {
	for num := blockFrom; num < blockTo; num++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		f, err := c.file(num)
		if err != nil {
			return 0, err
		}
		headerRLP, err := f.HeaderRLP(num)
		if err != nil {
			return 0, err
		}
		if err := collect(append([]byte{crypto.Keccak256Hash(headerRLP)[0]}, headerRLP...)); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

// dumpBodies continues the transaction count from the caller's seed, since the archive has none.
func (c *eraChain) dumpBodies(ctx context.Context, _ kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, firstTxNum firstKeyGetter, collect func([]byte) error, _ int, _ log.Lvl, _ log.Logger) (uint64, error) {
	txNum := firstTxNum(ctx)
	for num := blockFrom; num < blockTo; num++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		f, err := c.file(num)
		if err != nil {
			return 0, err
		}
		body, err := f.Body(num)
		if err != nil {
			return 0, err
		}
		stored := types.BodyForStorage{
			BaseTxnID:   types.BaseTxnID(txNum),
			TxCount:     types.TxCountToTxAmount(len(body.Transactions)),
			Uncles:      body.Uncles,
			Withdrawals: body.Withdrawals,
		}
		txNum = stored.BaseTxnID.LastSystemTx(stored.TxCount) + 1
		word, err := rlp.EncodeToBytes(stored)
		if err != nil {
			return 0, err
		}
		if err := collect(word); err != nil {
			return 0, err
		}
	}
	return txNum, nil
}

// dumpTxs spreads sender recovery, by far the costliest part of the import, over cores, a batch of
// blocks at a time within one file. Like DumpTxs it leaves a third of the cores to the compressor.
func (c *eraChain) dumpTxs(ctx context.Context, _ kv.RoDB, chainConfig *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, _ log.Lvl, _ log.Logger) (uint64, error) {
	pool := estimate.AlmostAllCPUs()
	if pool > 3 {
		pool = pool / 3 * 2
	}
	workers = max(1, min(workers, pool))

	batch := make([][][]byte, workers)
	for num := blockFrom; num < blockTo; {
		f, err := c.file(num)
		if err != nil {
			return 0, err
		}
		end := min(num+uint64(workers), blockTo, f.Last()+1)
		g, gctx := errgroup.WithContext(ctx)
		for i := range int(end - num) {
			g.Go(func() (err error) {
				batch[i], err = blockTxnWords(gctx, f, chainConfig, num+uint64(i))
				return err
			})
		}
		if err := g.Wait(); err != nil {
			return 0, err
		}
		for _, words := range batch[:end-num] {
			for _, word := range words {
				if err := collect(word); err != nil {
					return 0, err
				}
			}
		}
		num = end
	}
	return 0, nil
}

// blockTxnWords builds one block's transaction words: an empty word for the system transaction on
// each side, and in between the hash prefix, the recovered sender, and the envelope.
func blockTxnWords(ctx context.Context, f *era.Era1, chainConfig *chain.Config, num uint64) ([][]byte, error) {
	header, _, err := f.Header(num)
	if err != nil {
		return nil, err
	}
	body, err := f.Body(num)
	if err != nil {
		return nil, err
	}
	signer := types.MakeSigner(chainConfig, num, header.Time)
	words := make([][]byte, 1, len(body.Transactions)+2)
	for i, envelope := range body.Transactions {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		txn, err := types.DecodeTransaction(envelope)
		if err != nil {
			return nil, fmt.Errorf("era import: block %d txn %d: %w", num, i, err)
		}
		sender, err := txn.Sender(*signer)
		if err != nil {
			return nil, fmt.Errorf("era import: block %d txn %d sender: %w", num, i, err)
		}
		addr, hash := sender.Value(), txn.Hash()
		word := make([]byte, 0, 1+len(addr)+len(envelope))
		word = append(append(append(word, hash[0]), addr[:]...), envelope...)
		words = append(words, word)
	}
	return append(words, nil), nil
}
