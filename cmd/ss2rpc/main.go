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

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/erigontech/erigon/cmd/rpcdaemon/cli"
	"github.com/erigontech/erigon/cmd/utils"
	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/node/logging"
	"github.com/erigontech/erigon/rpc"
	"github.com/erigontech/erigon/rpc/jsonrpc"

	_ "github.com/erigontech/erigon/db/snaptype2" // hack
)

func main() {
	rootCtx, rootCancel := common.RootContext()
	root := &cobra.Command{
		Use:           "ss2rpc",
		Short:         "ss2rpc serves JSON-RPC straight from block snapshots, without an Erigon node",
		SilenceErrors: true,
	}
	root.AddCommand(serveCommand())
	err := root.ExecuteContext(rootCtx)
	rootCancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serveCommand() *cobra.Command {
	_, cfg := cli.RootCommand() // rpcdaemon's defaults for all that ss2rpc leaves as is; that command never runs
	var snapshotDir, listen, chain string
	cmd := &cobra.Command{
		Use:          "serve",
		Short:        "Serve block, receipt and log methods from the files in --snapshot-dir",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := cli.ConfigureSnapshotOnly(cfg, snapshotDir, listen, chain); err != nil {
				return err
			}
			logger := logging.SetupLoggerCmd("ss2rpc", cmd)
			ctx := cmd.Context()
			db, backend, txPool, mining, stateCache, blockReader, engine, ff, err := cli.SnapshotOnlyServices(ctx, cfg, logger)
			if err != nil {
				return err
			}
			defer db.Close()
			defer engine.Close()

			apiList := jsonrpc.APIList(db, backend, txPool, mining, ff, stateCache, blockReader, cfg, engine, logger, nil, nil)
			rpc.PreAllocateRPCMetricLabels(apiList)
			return cli.StartRpcServer(ctx, cfg, apiList, logger)
		},
	}
	cmd.Flags().StringVar(&snapshotDir, "snapshot-dir", "", "snapshots directory of a datadir: the block segments, with the receipt cache and log indexes of a node or of an era import")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:8545", "address to serve JSON-RPC over HTTP on")
	cmd.Flags().StringVar(&chain, utils.ChainFlag.Name, "", "chain of the segments; needed only when their block 0 is the genesis of more than one known chain")
	if err := cmd.MarkFlagRequired("snapshot-dir"); err != nil {
		panic(err)
	}
	utils.CobraFlags(cmd, logging.Flags)
	return cmd
}
