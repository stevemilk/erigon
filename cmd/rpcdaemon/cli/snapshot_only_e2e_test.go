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
	"encoding/json"
	"fmt"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/holiman/uint256"
	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/cmd/rpcdaemon/cli/httpcfg"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/common/crypto"
	"github.com/erigontech/erigon/common/dir"
	"github.com/erigontech/erigon/common/hexutil"
	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/era"
	"github.com/erigontech/erigon/db/era/eratest"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/rawdbv3"
	"github.com/erigontech/erigon/db/kv/temporal"
	"github.com/erigontech/erigon/db/snapcfg"
	"github.com/erigontech/erigon/db/snapshotsync/freezeblocks"
	"github.com/erigontech/erigon/db/snaptype"
	dbstate "github.com/erigontech/erigon/db/state"
	"github.com/erigontech/erigon/db/state/statecfg"
	"github.com/erigontech/erigon/execution/chain"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
	"github.com/erigontech/erigon/execution/execfinality"
	"github.com/erigontech/erigon/execution/execmodule/execmoduletester"
	"github.com/erigontech/erigon/execution/tests/blockgen"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node"
	"github.com/erigontech/erigon/rpc"
	"github.com/erigontech/erigon/rpc/jsonrpc"
)

// TestSnapshotOnlyServesNodeFiles executes a chain, freezes its blocks and builds its receipt cache
// into files, as a node leaves its datadir, then checks what ss2rpc serves from those files
// against the receipts execution produced.
func TestSnapshotOnlyServesNodeFiles(t *testing.T) {
	statecfg.EnableHistoricalRCache()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logger := log.New()

	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	sender := crypto.PubkeyToAddress(key.PublicKey)
	genesis := &types.Genesis{
		Config: registerableConfig(t, chain.TestChainBerlinConfig, "snapshotonly"),
		Alloc:  types.GenesisAlloc{sender: {Balance: new(big.Int).Exp(big.NewInt(10), big.NewInt(20), nil)}},
	}
	m := execmoduletester.New(t,
		execmoduletester.WithGenesisSpec(genesis),
		execmoduletester.WithKey(key),
		execmoduletester.WithEnableDomain(kv.RCacheDomain),
		execmoduletester.WithStepSize(64),
	)
	chainspec.RegisterChainSpec(genesis.Config.ChainName, chainspec.Spec{Name: genesis.Config.ChainName, GenesisHash: m.Genesis.Hash(), Genesis: genesis, Config: genesis.Config})

	signer := types.LatestSignerForChainID(nil)
	pack, err := m.GenerateChain(snaptype.Erigon2MinSegmentSize+10, func(i int, block *blockgen.BlockGen) {
		for j := range 2 {
			topic := logTopic(i, j)
			logOnCreate := append(append([]byte{0x7f}, topic[:]...), 0x60, 0x00, 0x60, 0x00, 0xa1, 0x00) // PUSH32 topic PUSH1 0 PUSH1 0 LOG1 STOP
			txn, err := types.SignTx(types.NewContractCreation(block.TxNonce(sender), uint256.NewInt(0), 100_000, uint256.NewInt(1), logOnCreate), *signer, key)
			require.NoError(t, err)
			block.AddTx(txn)
		}
		txn, err := types.SignTx(types.NewTransaction(block.TxNonce(sender), common.Address{1}, uint256.NewInt(1), 21_000, uint256.NewInt(1), nil), *signer, key)
		require.NoError(t, err)
		block.AddTx(txn)
	})
	require.NoError(t, err)
	require.NoError(t, m.InsertChain(pack))

	snCfg, _ := snapcfg.KnownCfg("mainnet")
	require.NoError(t, freezeblocks.DumpBlocks(ctx, 0, snaptype.Erigon2MinSegmentSize, m.ChainConfig, m.Dirs.Tmp, m.Dirs.Snap, m.DB, 1, log.LvlInfo, logger, m.BlockReader, snCfg, nil))
	var lastTxNum uint64
	require.NoError(t, m.DB.View(ctx, func(tx kv.Tx) (err error) {
		_, lastTxNum, err = rawdbv3.TxNums.Last(tx)
		return err
	}))
	db := m.DB.(*temporal.DB)
	agg := db.Agg().(*dbstate.Aggregator)
	<-agg.WaitForBuildAndMerge(ctx)
	require.NoError(t, db.BuildFiles2(ctx, 0, kv.Step(lastTxNum/agg.StepSize()), execfinality.NewContext(^uint64(0), ^uint64(0), 0, false, rawdbv3.TxNums), true))
	<-agg.WaitForBuildAndMerge(ctx)
	// A node writes its settings at first start; the tester sets the step size directly.
	settings := fmt.Sprintf("step_size = %d\nsteps_in_frozen_file = %d\n", agg.StepSize(), agg.StepsInFrozenFile())
	require.NoError(t, os.WriteFile(filepath.Join(m.Dirs.Snap, dbstate.ERIGONDB_SETTINGS_FILE), []byte(settings), 0o644))

	snapDir := filepath.Join(t.TempDir(), datadir.SnapDir)
	require.NoError(t, os.CopyFS(snapDir, os.DirFS(m.Dirs.Snap)))
	blockIndexes, err := filepath.Glob(filepath.Join(snapDir, "*.idx"))
	require.NoError(t, err)
	require.NotEmpty(t, blockIndexes)
	for _, f := range append(blockIndexes, filepath.Join(snapDir, "salt-blocks.txt")) {
		require.NoError(t, dir.RemoveFile(f), "peers share block segments without their indexes")
	}
	client := serveSnapshotOnly(t, ctx, snapDir, logger)
	entries, err := os.ReadDir(filepath.Dir(snapDir))
	require.NoError(t, err)
	require.Len(t, entries, 2, "beside the block indexes it builds, the datadir is only read and locked")
	call := func(result any, method string, args ...any) error {
		return client.CallContext(ctx, result, method, args...)
	}

	var head hexutil.Uint64
	require.NoError(t, call(&head, "eth_blockNumber"))
	require.Greater(t, uint64(head), uint64(snaptype.Erigon2MinSegmentSize)/2, "receipts are served from the files")
	var genesisBlock map[string]any
	require.NoError(t, call(&genesisBlock, "eth_getBlockByNumber", "0x0", false))
	require.Equal(t, m.Genesis.Hash().Hex(), genesisBlock["hash"])
	var unknown any
	require.NoError(t, call(&unknown, "eth_getTransactionByHash", common.Hash{1}))
	require.Nil(t, unknown, "a hash missing from the block files is looked up in an empty pool")

	for _, num := range []uint64{1, uint64(head) / 2, uint64(head)} {
		block, want := pack.Blocks[num-1], pack.Receipts[num-1]
		var got []map[string]any
		require.NoError(t, call(&got, "eth_getBlockReceipts", hexutil.Uint64(num)))
		require.Len(t, got, len(want), "block %d", num)
		for i, w := range want {
			txn := block.Transactions()[i]
			var receipt map[string]any
			require.NoError(t, call(&receipt, "eth_getTransactionReceipt", txn.Hash()))
			require.Equal(t, got[i], receipt, "block %d txn %d", num, i)
			require.Equal(t, hexutil.Uint64(w.Status).String(), receipt["status"])
			require.Equal(t, hexutil.Uint64(w.CumulativeGasUsed).String(), receipt["cumulativeGasUsed"])
			require.Len(t, receipt["logs"], len(w.Logs))
			if txn.GetTo() == nil {
				require.Equal(t, w.ContractAddress.Hex(), common.HexToAddress(receipt["contractAddress"].(string)).Hex())
			}
		}

		for j := range 2 {
			var byTopic []map[string]any
			filter := map[string]any{"fromBlock": hexutil.Uint64(num - 1), "toBlock": hexutil.Uint64(num), "topics": []any{logTopic(int(num-1), j)}}
			require.NoError(t, call(&byTopic, "eth_getLogs", filter))
			require.Len(t, byTopic, 1)
			require.Equal(t, block.Transactions()[j].Hash().Hex(), byTopic[0]["transactionHash"])

			var byAddress []map[string]any
			filter = map[string]any{"fromBlock": hexutil.Uint64(num), "toBlock": hexutil.Uint64(num), "address": want[j].ContractAddress}
			require.NoError(t, call(&byAddress, "eth_getLogs", filter))
			require.Len(t, byAddress, 1)
		}
	}

	var receipts []map[string]any
	require.ErrorContains(t, call(&receipts, "eth_getBlockReceipts", hexutil.Uint64(head+1)), "not executed",
		"past the head the receipts are refused rather than re-executed")
	var balance any
	require.Error(t, call(&balance, "eth_getBalance", sender, "latest"), "state methods are not served")
	var fees any
	var coded rpc.Error
	require.ErrorAs(t, call(&fees, "eth_feeHistory", 2, head, []float64{50}), &coded)
	require.Equal(t, -32004, coded.ErrorCode(), "a served method that reaches state is not supported rather than answered from empty state")
}

// TestSnapshotOnlyServesEraImport serves what era-import leaves in a bare datadir: epoch block
// segments built from the archive, and the receipts and log indexes it imported. No node runs.
func TestSnapshotOnlyServesEraImport(t *testing.T) {
	ctx := t.Context()
	logger := log.New()

	eraDir := t.TempDir()
	archive := eratest.NewChain(t, 0)
	for range 2 {
		archive.WriteFile(t, eraDir, era.MaxBlocksPerFile, 2)
	}
	cfg := registerableConfig(t, chainspec.Mainnet.Config, "eraimport")
	chainspec.RegisterChainSpec(cfg.ChainName, chainspec.Spec{Name: cfg.ChainName, GenesisHash: archive.Genesis(), Genesis: &types.Genesis{Config: cfg}, Config: cfg})

	dirs := datadir.New(t.TempDir())
	receiptDirs := dirs.EraReceipts()
	require.NoError(t, os.MkdirAll(receiptDirs.Snap, 0o755))
	settings := []byte("step_size = 5000\nsteps_in_frozen_file = 4\n")
	require.NoError(t, os.WriteFile(filepath.Join(receiptDirs.Snap, dbstate.ERIGONDB_SETTINGS_FILE), settings, 0o644))
	// The archive is named after mainnet, and the blocks follow its rules.
	snCfg, _ := snapcfg.KnownCfg("mainnet")
	require.NoError(t, freezeblocks.ImportEra(ctx, eraDir, dirs, chainspec.Mainnet.Config, snCfg, archive.Genesis(), 1, logger))
	require.NoError(t, freezeblocks.ImportEraReceipts(ctx, eraDir, dirs, chainspec.Mainnet.Config, logger))
	segments, err := filepath.Glob(filepath.Join(dirs.Snap, "*-ep.seg"))
	require.NoError(t, err)
	require.NotEmpty(t, segments)

	client := serveSnapshotOnly(t, ctx, dirs.Snap, logger)
	call := func(result any, method string, args ...any) error {
		return client.CallContext(ctx, result, method, args...)
	}

	var head hexutil.Uint64
	require.NoError(t, call(&head, "eth_blockNumber"))
	require.Greater(t, uint64(head), uint64(era.MaxBlocksPerFile), "receipts reach into the second archive file")
	for _, num := range []uint64{1, uint64(head) / 2, uint64(head)} {
		var receipts []map[string]any
		require.NoError(t, call(&receipts, "eth_getBlockReceipts", hexutil.Uint64(num)))
		require.Len(t, receipts, 2, "block %d", num)
		for i, r := range receipts {
			require.Equal(t, "0x1", r["status"])
			require.Equal(t, hexutil.Uint64((i+1)*21_000).String(), r["cumulativeGasUsed"])
			require.Equal(t, i%2 == 0, r["contractAddress"] != nil, "even transactions create a contract")
			logs := r["logs"].([]any)
			require.Len(t, logs, 1)
			require.Equal(t, strings.ToLower(eratest.LogAddress(num, i).Hex()), logs[0].(map[string]any)["address"])
		}

		var byTopic []map[string]any
		filter := map[string]any{"fromBlock": hexutil.Uint64(num), "toBlock": hexutil.Uint64(num), "topics": []any{eratest.LogTopic(num, 1)}}
		require.NoError(t, call(&byTopic, "eth_getLogs", filter))
		require.Len(t, byTopic, 1)
		require.Equal(t, receipts[1]["transactionHash"], byTopic[0]["transactionHash"])
	}

	var receipts []map[string]any
	require.ErrorContains(t, call(&receipts, "eth_getBlockReceipts", hexutil.Uint64(head+1)), "not executed",
		"blocks past the receipt files are refused")
}

func logTopic(block, i int) common.Hash {
	return common.Hash{0xbb, byte(block >> 8), byte(block), byte(i)}
}

// registerableConfig copies base under a chain name of its own, so that the test chain can be
// registered and found by its genesis.
func registerableConfig(t *testing.T, base *chain.Config, name string) *chain.Config {
	t.Helper()
	raw, err := json.Marshal(base)
	require.NoError(t, err)
	cfg := &chain.Config{}
	require.NoError(t, json.Unmarshal(raw, cfg))
	cfg.ChainName, cfg.ChainID = name, uint256.NewInt(1_337_001)
	return cfg
}

// defaultHttpCfg is rpcdaemon's defaults. RootCommand registers process-wide flags, so it runs once.
var defaultHttpCfg = sync.OnceValue(func() httpcfg.HttpCfg {
	_, cfg := RootCommand()
	return *cfg
})

// serveSnapshotOnly wires the services the way ss2rpc does and returns a client over HTTP.
func serveSnapshotOnly(t *testing.T, ctx context.Context, snapDir string, logger log.Logger) *rpc.Client {
	t.Helper()
	defaults := defaultHttpCfg()
	cfg := &defaults
	require.NoError(t, ConfigureSnapshotOnly(cfg, snapDir, "127.0.0.1:0", ""))

	db, eth, txPool, mining, stateCache, br, engine, ff, err := SnapshotOnlyServices(ctx, cfg, logger)
	require.NoError(t, err)
	t.Cleanup(db.Close)

	srv := rpc.NewServer(cfg.RpcBatchConcurrency, false, false, false, logger, cfg.RPCSlowLogThreshold)
	allowList, err := allowListForRPC(cfg)
	require.NoError(t, err)
	srv.SetAllowList(allowList)
	apis := jsonrpc.APIList(db, eth, txPool, mining, ff, stateCache, br, cfg, engine, logger, nil, nil)
	require.NoError(t, node.RegisterApisFromWhitelist(apis, cfg.API, srv, false, logger))
	httpSrv := httptest.NewServer(srv)
	t.Cleanup(httpSrv.Close)
	client, err := rpc.DialHTTP(httpSrv.URL, logger)
	require.NoError(t, err)
	t.Cleanup(client.Close)
	return client
}
