// Copyright 2024 The Erigon Authors
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

package rpctest

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"time"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon/eth/ethconfig/estimate"
	"golang.org/x/sync/errgroup"
)

// BenchEthGetLogs compares response of Erigon with Geth
// but also can be used for comparing RPCDaemon with Geth or infura
// parameters:
// needCompare - if false - doesn't call Erigon and doesn't compare responses
//
//	false value - to generate vegeta files, it's faster but we can generate vegeta files for Geth and Erigon
//	recordFile stores all eth_getlogs returned with success
//	errorFile stores information when erigon and geth doesn't return same data
func BenchEthGetLogs(erigonURL, gethURL string, needCompare bool, blockFrom uint64, blockTo uint64, recordFile string, errorFile string) error {
	setRoutes(erigonURL, gethURL)

	var rec *bufio.Writer
	if recordFile != "" {
		f, err := os.Create(recordFile)
		if err != nil {
			return fmt.Errorf("Cannot create file %s for recording: %v\n", recordFile, err)
		}
		defer f.Close()
		rec = bufio.NewWriter(f)
		defer rec.Flush()
	}
	var errs *bufio.Writer
	if errorFile != "" {
		ferr, err := os.Create(errorFile)
		if err != nil {
			return fmt.Errorf("Cannot create file %s for error output: %v\n", errorFile, err)
		}
		defer ferr.Close()
		errs = bufio.NewWriter(ferr)
		defer errs.Flush()
	}

	var resultsCh chan CallResult = nil
	if !needCompare {
		resultsCh = make(chan CallResult, 1000)
		defer close(resultsCh)
		go vegetaWrite(true, []string{"debug_getModifiedAccountsByNumber", "eth_getLogs"}, resultsCh)
	}

	var res CallResult
	reqGen := &RequestGenerator{}

	var blockNumber EthBlockNumber
	res = reqGen.Erigon("eth_blockNumber", reqGen.blockNumber(), &blockNumber)
	if res.Err != nil {
		return fmt.Errorf("Could not get block number: %v\n", res.Err)
	}
	if blockNumber.Error != nil {
		return fmt.Errorf("Error getting block number: %d %s\n", blockNumber.Error.Code, blockNumber.Error.Message)
	}
	fmt.Printf("Last block: %d\n", blockNumber.Number)

	prevBn := blockFrom
	rnd := rand.New(rand.NewSource(42)) // nolint:gosec
	for bn := blockFrom + 100; bn < blockTo; bn += 100 {

		// Checking modified accounts

		var mag DebugModifiedAccounts
		res = reqGen.Erigon("debug_getModifiedAccountsByNumber", reqGen.getModifiedAccountsByNumber(prevBn, bn), &mag)
		if res.Err != nil {
			return fmt.Errorf("Could not get modified accounts (Erigon): %v\n", res.Err)
		}
		if mag.Error != nil {
			return fmt.Errorf("Error getting modified accounts (Erigon): %d %s\n", mag.Error.Code, mag.Error.Message)
		}
		if res.Err == nil && mag.Error == nil {
			accountSet := extractAccountMap(&mag)
			for account := range accountSet {

				request := reqGen.getLogs(prevBn, bn, account)
				errCtx := fmt.Sprintf("account %x blocks %d-%d", account, prevBn, bn)
				if err := requestAndCompare(request, "eth_getLogs", errCtx, reqGen, needCompare, rec, errs, resultsCh,
					/* insertOnlyIfSuccess */ false); err != nil {
					fmt.Println(err)
					return err
				}
				topics := getTopics(res.Result)
				// All combination of account and one topic
				for _, topic := range topics {

					request = reqGen.getLogs1(prevBn, bn+10000, account, topic)
					errCtx := fmt.Sprintf("account %x topic %x blocks %d-%d", account, topic, prevBn, bn)
					if err := requestAndCompare(request, "eth_getLogs", errCtx, reqGen, needCompare, rec, errs, resultsCh,
						/* insertOnlyIfSuccess */ false); err != nil {
						fmt.Println(err)
						return err
					}
				}
				// Random combinations of two topics
				if len(topics) >= 2 {
					idx1 := rnd.Int31n(int32(len(topics)))
					idx2 := rnd.Int31n(int32(len(topics) - 1))
					if idx2 >= idx1 {
						idx2++
					}

					request = reqGen.getLogs2(prevBn, bn+100000, account, topics[idx1], topics[idx2])
					errCtx := fmt.Sprintf("account %x topic1 %x topic2 %x blocks %d-%d", account, topics[idx1], topics[idx2], prevBn, bn)
					if err := requestAndCompare(request, "eth_getLogs", errCtx, reqGen, needCompare, rec, errs, resultsCh,
						/* insertOnlyIfSuccess */ false); err != nil {
						fmt.Println(err)
						return err
					}
				}
			}
		}
		fmt.Printf("Done blocks %d-%d, modified accounts: %d\n", prevBn, bn, len(mag.Result))
		prevBn = bn
	}
	return nil
}

func EthGetLogsInvariants(ctx context.Context, erigonURL, gethURL string, needCompare bool, blockFrom, blockTo uint64) error {
	setRoutes(erigonURL, gethURL)

	reqGen := &RequestGenerator{}

	var blockNumber EthBlockNumber
	res := reqGen.Erigon("eth_blockNumber", reqGen.blockNumber(), &blockNumber)
	if res.Err != nil {
		return fmt.Errorf("Could not get block number: %v\n", res.Err)
	}
	if blockNumber.Error != nil {
		return fmt.Errorf("Error getting block number: %d %s\n", blockNumber.Error.Code, blockNumber.Error.Message)
	}
	fmt.Printf("EthGetLogsInvariants: starting %d-%d, latestBlock=%d\n", blockFrom, blockTo, blockNumber.Number)
	logEvery := time.NewTicker(20 * time.Second)
	defer logEvery.Stop()

	noDuplicates := func(logs []Log) error {
		if len(logs) <= 1 {
			return nil
		}
		var indices []uint64
		for i := 0; i < len(logs); i++ {
			indices = append(indices, uint64(logs[i].Index))
		}
		slices.Sort(indices)
		for i := 1; i < len(logs); i++ {
			if indices[i-1] == indices[i] {
				return fmt.Errorf("duplicated log_index %d", indices[i])
			}
		}
		return nil
	}

	for bn := blockFrom; bn < blockTo; {
		batchEnd := min(bn+10, blockTo)
		eg := &errgroup.Group{}
		eg.SetLimit(estimate.AlmostAllCPUs())
		//eg.SetLimit(1)
		for ; bn < batchEnd; bn++ {
			bn := bn
			//eg.Go(func() error {
			var resp EthGetLogs
			res := reqGen.Erigon("eth_getLogs", reqGen.getLogsNoFilters(bn, bn), &resp)
			if res.Err != nil {
				return fmt.Errorf("Could not get modified accounts (Erigon): %v\n", res.Err)
			}
			if resp.Error != nil {
				return fmt.Errorf("Error getting modified accounts (Erigon): %d %s\n", resp.Error.Code, resp.Error.Message)
			}
			if err := noDuplicates(resp.Result); err != nil {
				return fmt.Errorf("eth_getLogs: at blockNum=%d %w", bn, err)
			}

			sawAddr := map[common.Address]struct{}{} // don't check same addr in this block
			sawTopic := map[common.Hash]struct{}{}
			for _, l := range resp.Result {
				if _, ok := sawAddr[l.Address]; ok {
					continue
				}
				sawAddr[l.Address] = struct{}{}

				res = reqGen.Erigon("eth_getLogs", reqGen.getLogs(bn, bn, l.Address), &resp)
				if res.Err != nil {
					return fmt.Errorf("Could not get modified accounts (Erigon): %v\n", res.Err)
				}
				if resp.Error != nil {
					return fmt.Errorf("Error getting modified accounts (Erigon): %d %s\n", resp.Error.Code, resp.Error.Message)
				}
				//invariant1: if `log` visible without filter - then must be visible with filter. (in another words: `address` must be indexed well)
				if len(resp.Result) == 0 {
					return fmt.Errorf("eth_getLogs: at blockNum=%d account %x not indexed", bn, l.Address)
				}

				if err := noDuplicates(resp.Result); err != nil {
					return fmt.Errorf("eth_getLogs: at blockNum=%d and addr %x %w", bn, l.Address, err)
				}

				//invariant2: if `log` visible without filter - then must be visible with filter. (in another words: `topic` must be indexed well)
				if len(l.Topics) == 0 {
					continue
				}

				if _, ok := sawTopic[l.Topics[0]]; ok {
					continue
				}
				sawTopic[l.Topics[0]] = struct{}{}

				res = reqGen.Erigon("eth_getLogs", reqGen.getLogs1(bn, bn, l.Address, l.Topics[0]), &resp)
				if res.Err != nil {
					return fmt.Errorf("Could not get modified accounts (Erigon): %v\n", res.Err)
				}
				if resp.Error != nil {
					return fmt.Errorf("Error getting modified accounts (Erigon): %d %s\n", resp.Error.Code, resp.Error.Message)
				}
				if len(resp.Result) == 0 {
					return fmt.Errorf("eth_getLogs: at blockNum=%d account %x, topic %x not indexed", bn, l.Address, l.Topics[0])
				}
				if err := noDuplicates(resp.Result); err != nil {
					return fmt.Errorf("eth_getLogs: at blockNum=%d and topic %x %w", bn, l.Topics[0], err)
				}
			}

			select {
			case <-logEvery.C:
				log.Info("[ethGetLogsInvariants]", "block_num", bn)
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			//return nil
			//})
		}

		if err := eg.Wait(); err != nil {
			return err
		}
	}
	return nil
}
