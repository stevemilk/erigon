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
	"math"
	"os"
	"path/filepath"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/estimate"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/seg"
	"github.com/erigontech/erigon/db/snapcfg"
	"github.com/erigontech/erigon/db/snaptype"
	"github.com/erigontech/erigon/db/snaptype2"
	"github.com/erigontech/erigon/execution/chain"
	"github.com/erigontech/erigon/execution/rlp"
	"github.com/erigontech/erigon/execution/types"
	"golang.org/x/sync/errgroup"
)

// eraSet is an unbroken run of archive files. It does not have to start at epoch 0: a run that
// continues segments already built takes the transaction count and the parent hash from them, so
// the files behind it are of no further use and can be deleted as the import moves along.
type eraSet struct {
	first    int // epoch of paths[0]
	paths    []string
	open     era.Reader
	openN    int // epoch of the file in open, -1 when none
	verified map[int]bool
	// lastHash is the hash of block nextBlock-1, so verification continues as one chain. A resumed
	// run seeds them from the segments it skips rather than from the archive.
	lastHash  common.Hash
	nextBlock uint64
	genesis   common.Hash
	logger    log.Logger
}

func openEraSet(eraDir, network string, genesis common.Hash, logger log.Logger) (*eraSet, error) {
	entries, err := os.ReadDir(eraDir)
	if err != nil {
		return nil, err
	}
	byEpoch := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) != ".era1" {
			continue // .ere is not readable yet; skipping it lets the era1 prefix still import
		}
		parsed, ok := era.ParseName(name)
		if !ok || parsed.Network != network {
			continue
		}
		if prev, dup := byEpoch[parsed.Epoch]; dup {
			return nil, fmt.Errorf("era import: epoch %d is in both %s and %s", parsed.Epoch, prev, name)
		}
		byEpoch[parsed.Epoch] = filepath.Join(eraDir, name)
	}
	if len(byEpoch) == 0 {
		return nil, fmt.Errorf("era import: no %s archive files in %s", network, eraDir)
	}

	first := math.MaxInt
	for epoch := range byEpoch {
		first = min(first, epoch)
	}
	s := &eraSet{first: first, paths: make([]string, len(byEpoch)), openN: -1, verified: map[int]bool{}, genesis: genesis, logger: logger}
	for i := range s.paths {
		p, ok := byEpoch[first+i]
		if !ok {
			return nil, fmt.Errorf("era import: epoch %d is missing; the files have to be an unbroken run", first+i)
		}
		s.paths[i] = p
	}
	return s, nil
}

// from is the first block the archive covers.
func (s *eraSet) from() uint64 { return uint64(s.first) * era.MaxBlocksPerFile }

// end is the block the archive stops at, read from the last file's index directly so that planning
// does not depend on verifying it first.
func (s *eraSet) end() (uint64, error) {
	r, err := era.Open(s.paths[len(s.paths)-1])
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return r.Last() + 1, nil
}

// readerFor returns the file holding blockNum, keeping only the current one open since callers walk
// block numbers in order.
func (s *eraSet) readerFor(blockNum uint64) (era.Reader, error) {
	n := int(blockNum/era.MaxBlocksPerFile) - s.first
	if n < 0 || n >= len(s.paths) {
		return nil, fmt.Errorf("era import: block %d is outside the archive", blockNum)
	}
	if s.openN == n {
		return s.open, nil
	}
	if s.open != nil {
		s.open.Close()
		s.open, s.openN = nil, -1
	}
	r, err := era.Open(s.paths[n])
	if err != nil {
		return nil, err
	}
	// Files are opened in order, so the parent hash carries over and the whole run is verified as
	// one chain, anchored on genesis.
	if !s.verified[n] {
		if r.Start() != s.nextBlock {
			r.Close()
			return nil, fmt.Errorf("era import: %s starts at block %d, but the chain so far ends at %d", filepath.Base(s.paths[n]), r.Start(), s.nextBlock)
		}
		last, err := era.Verify(r, s.lastHash, s.genesis)
		if err != nil {
			r.Close()
			return nil, err
		}
		s.verified[n] = true
		s.lastHash, s.nextBlock = last, r.Last()+1
		s.logger.Info("[era-import] verified", "file", filepath.Base(s.paths[n]))
	}
	s.open, s.openN = r, n
	return r, nil
}

// resumeAt takes the chain state from a range already on disk, so a resumed run does not have to
// re-read the archive files behind it.
func (s *eraSet) resumeAt(block uint64, hash common.Hash) {
	s.lastHash, s.nextBlock = hash, block
}

func (s *eraSet) Close() {
	if s.open != nil {
		s.open.Close()
		s.open, s.openN = nil, -1
	}
}

func (s *eraSet) headerRLP(num uint64) ([]byte, error) {
	r, err := s.readerFor(num)
	if err != nil {
		return nil, err
	}
	return r.HeaderRLP(num)
}

func (s *eraSet) body(num uint64) (types.RawBody, error) {
	r, err := s.readerFor(num)
	if err != nil {
		return types.RawBody{}, err
	}
	raw, err := r.BodyRLP(num)
	if err != nil {
		return types.RawBody{}, err
	}
	var body types.RawBody
	if err := rlp.DecodeBytes(raw, &body); err != nil {
		return types.RawBody{}, fmt.Errorf("era import: block %d body: %w", num, err)
	}
	return body, nil
}

func (s *eraSet) dumpHeaders(ctx context.Context, _ kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, _ int, _ log.Lvl, _ log.Logger) (uint64, error) {
	for num := blockFrom; num < blockTo; num++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		headerRLP, err := s.headerRLP(num)
		if err != nil {
			return 0, err
		}
		word := make([]byte, len(headerRLP)+1)
		word[0] = crypto.Keccak256Hash(headerRLP)[0]
		copy(word[1:], headerRLP)
		if err := collect(word); err != nil {
			return 0, err
		}
	}
	return 0, nil
}

func (s *eraSet) dumpBodies(ctx context.Context, _ kv.RoDB, _ *chain.Config, blockFrom, blockTo uint64, firstTxNum firstKeyGetter, collect func([]byte) error, _ int, _ log.Lvl, _ log.Logger) (uint64, error) {
	// The archive has no BaseTxnID, so it is continued from the caller's seed.
	lastTxNum := firstTxNum(ctx)
	for num := blockFrom; num < blockTo; num++ {
		if err := ctx.Err(); err != nil {
			return lastTxNum, err
		}
		body, err := s.body(num)
		if err != nil {
			return lastTxNum, err
		}
		stored := types.BodyForStorage{
			BaseTxnID:   types.BaseTxnID(lastTxNum),
			TxCount:     types.TxCountToTxAmount(len(body.Transactions)),
			Uncles:      body.Uncles,
			Withdrawals: body.Withdrawals,
		}
		lastTxNum = stored.BaseTxnID.LastSystemTx(stored.TxCount) + 1

		word, err := rlp.EncodeToBytes(stored)
		if err != nil {
			return lastTxNum, err
		}
		if err := collect(word); err != nil {
			return lastTxNum, err
		}
	}
	return lastTxNum, nil
}

func (s *eraSet) dumpTxs(ctx context.Context, _ kv.RoDB, chainConfig *chain.Config, blockFrom, blockTo uint64, _ firstKeyGetter, collect func([]byte) error, workers int, _ log.Lvl, _ log.Logger) (uint64, error) {
	// Recovering a sender costs far more than everything else the import does put together, so it is
	// the one part spread over cores. Blocks are done in batches and their words collected in order,
	// since the segment is a flat sequence. Like DumpTxs, this leaves a third of the cores to the
	// compressor running alongside it.
	pool := estimate.AlmostAllCPUs()
	if pool > 3 {
		pool = pool / 3 * 2
	}
	workers = max(1, min(workers, pool))

	batch := make([][][]byte, workers)
	for num := blockFrom; num < blockTo; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		// A batch stays inside one file so the reader is opened, and verified, in block order.
		fileEnd := (num/era.MaxBlocksPerFile + 1) * era.MaxBlocksPerFile
		end := min(num+uint64(workers), blockTo, fileEnd)
		r, err := s.readerFor(num)
		if err != nil {
			return 0, err
		}

		g, gctx := errgroup.WithContext(ctx)
		for i := range int(end - num) {
			g.Go(func() error {
				words, err := blockTxnWords(gctx, r, chainConfig, num+uint64(i))
				batch[i] = words
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
func blockTxnWords(ctx context.Context, r era.Reader, chainConfig *chain.Config, num uint64) ([][]byte, error) {
	headerRLP, err := r.HeaderRLP(num)
	if err != nil {
		return nil, err
	}
	var header types.Header
	if err := rlp.DecodeBytes(headerRLP, &header); err != nil {
		return nil, fmt.Errorf("era import: block %d header: %w", num, err)
	}
	bodyRLP, err := r.BodyRLP(num)
	if err != nil {
		return nil, err
	}
	var body types.RawBody
	if err := rlp.DecodeBytes(bodyRLP, &body); err != nil {
		return nil, fmt.Errorf("era import: block %d body: %w", num, err)
	}
	// Signature rules are fork-dependent and the archive has no sender, so each transaction is
	// recovered against the signer for its own block.
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

		word := make([]byte, 0, 21+len(envelope))
		word = append(word, hash[0])
		word = append(word, addr[:]...)
		word = append(word, envelope...)
		words = append(words, word)
	}
	return append(words, nil), nil
}

// checkNoOverlap refuses to write a range that another segment already covers part of. The plan
// depends on how many blocks the archive holds, so a run over a grown archive tiles differently and
// would otherwise leave two segments covering the same blocks.
func checkNoOverlap(names []string, from, to uint64) error {
	for _, name := range names {
		info, _, ok := snaptype.ParseFileName("", name)
		if !ok || !info.Epoch || info.Ext != ".seg" {
			continue
		}
		if info.From == from && info.To == to {
			continue // the range itself, about to be rewritten
		}
		if info.From < to && from < info.To {
			return fmt.Errorf("era import: %s already covers part of [%d,%d); remove the epoch segments and import again", name, from, to)
		}
	}
	return nil
}

// builtPrefix returns the epoch ranges already built, contiguous from block 0.
func builtPrefix(dirs datadir.Dirs, names []string, logger log.Logger) ([][2]uint64, error) {
	all, err := snaptype.Segments(dirs.Snap)
	if err != nil {
		return nil, err
	}
	// Sorted and free of overlaps, so a merged segment and the ones it covers cannot both be walked.
	headers, _ := classifyByType(all, snaptype2.Enums.Headers)

	var out [][2]uint64
	at := uint64(0)
	for i := range headers {
		f := &headers[i]
		if f.From != at {
			break
		}
		complete, err := rangeComplete(dirs, names, f.From, f.To, logger)
		if err != nil || !complete {
			return out, err
		}
		out = append(out, [2]uint64{f.From, f.To})
		at = f.To
	}
	return out, nil
}

// rangeComplete reports whether all three types have both their data and their indexes. They are
// written one after another, so an interrupted run can leave a range half built.
func rangeComplete(dirs datadir.Dirs, names []string, from, to uint64, logger log.Logger) (bool, error) {
	for _, t := range snaptype2.BlockSnapshotTypes {
		info := t.FileInfo(dirs.Snap, true, from, to)
		if ok, err := dir.FileExist(info.Path); err != nil || !ok {
			return false, err
		}
		if !t.HasIndexFiles(info, names, logger) {
			return false, nil
		}
	}
	return true, nil
}

// builtRange reports whether the range is already on disk, and what the next range has to carry on
// from: the transaction count it ends at and the hash of its last block.
func builtRange(dirs datadir.Dirs, names []string, from, to uint64, want uint64, logger log.Logger) (nextTxNum uint64, lastHash common.Hash, built bool, err error) {
	if complete, err := rangeComplete(dirs, names, from, to, logger); err != nil || !complete {
		return 0, common.Hash{}, false, err
	}
	bodies := snaptype2.Bodies.FileInfo(dirs.Snap, true, from, to)
	headers := snaptype2.Headers.FileInfo(dirs.Snap, true, from, to)

	// The archive does not carry the transaction count, so it is only recoverable from the bodies
	// already written. Reading it back also checks that they continue the ranges before them.
	d, err := seg.NewDecompressor(bodies.Path)
	if err != nil {
		return 0, common.Hash{}, false, err
	}
	defer d.Close()
	if uint64(d.Count()) != to-from {
		return 0, common.Hash{}, false, fmt.Errorf("era import: %s holds %d blocks, not %d", bodies.Name(), d.Count(), to-from)
	}

	// Only the first and last blocks say anything: BaseTxnID is absolute, so the first has to
	// continue the ranges before it and the last says where the next one starts.
	firstBody, err := decodeBody(d, firstWord(d))
	if err != nil {
		return 0, common.Hash{}, false, err
	}
	if uint64(firstBody.BaseTxnID) != want {
		return 0, common.Hash{}, false, fmt.Errorf("era import: %s starts at transaction %d, but the ranges before it end at %d", bodies.Name(), firstBody.BaseTxnID, want)
	}
	lastBody, err := decodeBody(d, lastWord(d))
	if err != nil {
		return 0, common.Hash{}, false, err
	}

	// The last header carries the hash the next range has to follow on from.
	hd, err := seg.NewDecompressor(headers.Path)
	if err != nil {
		return 0, common.Hash{}, false, err
	}
	defer hd.Close()
	if uint64(hd.Count()) != to-from {
		return 0, common.Hash{}, false, fmt.Errorf("era import: %s holds %d blocks, not %d", headers.Name(), hd.Count(), to-from)
	}
	// The word is the first byte of the hash then the header RLP.
	word := lastWord(hd)
	if len(word) == 0 {
		return 0, common.Hash{}, false, fmt.Errorf("era import: %s ends with an empty word", headers.Name())
	}
	return lastBody.BaseTxnID.LastSystemTx(lastBody.TxCount) + 1, crypto.Keccak256Hash(word[1:]), true, nil
}

// firstWord and lastWord read one end of a segment, skipping over the rest rather than
// decompressing it. Both assume the caller has checked the segment is not empty.
func firstWord(d *seg.Decompressor) []byte {
	word, _ := d.MakeGetter().Next(nil)
	return word
}

func lastWord(d *seg.Decompressor) []byte {
	g := d.MakeGetter()
	for range d.Count() - 1 {
		g.Skip()
	}
	word, _ := g.Next(nil)
	return word
}

func decodeBody(d *seg.Decompressor, word []byte) (types.BodyForStorage, error) {
	var body types.BodyForStorage
	if err := rlp.DecodeBytes(word, &body); err != nil {
		return body, fmt.Errorf("era import: %s: %w", d.FileName(), err)
	}
	return body, nil
}

// ImportEra builds epoch block segments from a run of archive files. genesis anchors the run to a
// real chain: without it the files only have to be consistent with each other, which a fabricated
// chain also manages.
func ImportEra(ctx context.Context, eraDir string, dirs datadir.Dirs, chainConfig *chain.Config, snCfg *snapcfg.Cfg, genesis common.Hash, workers int, logger log.Logger) error {
	if !snaptype2.RegimeFor(chainConfig) {
		return fmt.Errorf("era import: %s does not use epoch segments", chainConfig.ChainName)
	}
	if genesis == (common.Hash{}) {
		return errors.New("era import: no genesis hash to anchor the archive on")
	}
	// A datadir that has not downloaded snapshots yet has no salt file, and indexes need one.
	if _, err := snaptype.LoadSalt(dirs.Snap, true, logger); err != nil {
		return err
	}
	set, err := openEraSet(eraDir, chainConfig.ChainName, genesis, logger)
	if err != nil {
		return err
	}
	defer set.Close()

	end, err := set.end()
	if err != nil {
		return err
	}
	names, err := dir.ListFiles(dirs.Snap)
	if err != nil {
		return err
	}
	for i := range names {
		names[i] = filepath.Base(names[i])
	}

	// Continue the tiling already on disk rather than working out a fresh one: how a run from block
	// 0 tiles depends on how many blocks the archive holds, so an archive that has grown since the
	// last run would be cut differently and the two cuts would cover the same blocks twice.
	plan, err := builtPrefix(dirs, names, logger)
	if err != nil {
		return err
	}
	coveredTo := uint64(0)
	if len(plan) > 0 {
		coveredTo = plan[len(plan)-1][1]
	}
	// The archive has to pick up where the segments leave off. It may start earlier — those files
	// are simply not needed — but a gap means the blocks in between exist nowhere.
	if set.from() > coveredTo {
		return fmt.Errorf("era import: the segments end at block %d but the archive starts at %d", coveredTo, set.from())
	}
	rest, tailFrom := planEpochSegments(coveredTo, end, snCfg)
	plan = append(plan, rest...)
	if len(plan) == 0 {
		return fmt.Errorf("era import: blocks up to %d are not enough to fill one segment", end)
	}
	logger.Info("[era-import] planned", "end", end, "segments", len(plan), "already-built", len(plan)-len(rest), "tail-from", tailFrom)

	var firstTxNum uint64
	for _, r := range plan {
		from, to := r[0], r[1]
		// Skipping only works up to a file boundary: the chain is picked up from the segments, and
		// the next file opened still has to be verified from its own first block.
		if to%era.MaxBlocksPerFile == 0 {
			if next, lastHash, built, err := builtRange(dirs, names, from, to, firstTxNum, logger); err != nil {
				return err
			} else if built {
				firstTxNum = next
				set.resumeAt(to, lastHash)
				logger.Info("[era-import] already built, skipping", "from", from, "to", to)
				continue
			}
		}
		if err := checkNoOverlap(names, from, to); err != nil {
			return err
		}
		seed := func(context.Context) uint64 { return firstTxNum }
		dump := func(t snaptype.Type, d dumpFunc) (uint64, error) {
			return dumpRange(ctx, t.FileInfo(dirs.Snap, true, from, to), d, seed, nil, chainConfig, dirs.Tmp, workers, log.LvlInfo, logger, nil)
		}

		if _, err := dump(snaptype2.Headers, set.dumpHeaders); err != nil {
			return err
		}
		lastTxNum, err := dump(snaptype2.Bodies, set.dumpBodies)
		if err != nil {
			return err
		}
		if _, err := dump(snaptype2.Transactions, set.dumpTxs); err != nil {
			return err
		}
		firstTxNum = lastTxNum // dumpBodies already returns the next block's base
		logger.Info("[era-import] segment done", "from", from, "to", to)
	}
	return nil
}
