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
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/erigontech/erigon/common"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/version"
	"github.com/erigontech/erigon/execution/types"
	"github.com/erigontech/erigon/node/gointerfaces/remoteproto"
	"github.com/erigontech/erigon/node/gointerfaces/txpoolproto"
	"github.com/erigontech/erigon/p2p"
)

var errNotServedSnapshotOnly = errors.New("not served by ss2rpc")

// snapshotOnlyBackend stands in for the execution-service backend when there is no node to talk to.
type snapshotOnlyBackend struct {
	chainID uint64
}

func (b *snapshotOnlyBackend) Syncing(context.Context) (*remoteproto.SyncingReply, error) {
	return &remoteproto.SyncingReply{}, nil
}

func (b *snapshotOnlyBackend) NetVersion(context.Context) (uint64, error) { return b.chainID, nil }

func (b *snapshotOnlyBackend) ClientVersion(context.Context) (string, error) {
	return common.MakeName("erigon", version.VersionWithCommit(version.GitCommit)), nil
}

func (b *snapshotOnlyBackend) BlockWithSenders(context.Context, kv.Getter, common.Hash, uint64) (*types.Block, []common.Address, error) {
	return nil, nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) PendingBlock(context.Context) (*types.Block, error) { return nil, nil }

func (b *snapshotOnlyBackend) Etherbase(context.Context) (common.Address, error) {
	return common.Address{}, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) NetPeerCount(context.Context) (uint64, error) {
	return 0, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) ProtocolVersion(context.Context) (uint64, error) {
	return 0, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) Subscribe(context.Context, func(*remoteproto.SubscribeReply)) error {
	return errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) SubscribeLogs(context.Context, func(*remoteproto.SubscribeLogsReply), *atomic.Value) error {
	return errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) SubscribeReceipts(context.Context, func(*remoteproto.SubscribeReceiptsReply), func(func(*remoteproto.ReceiptsFilterRequest) error)) error {
	return errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) NodeInfo(context.Context, uint32) ([]p2p.NodeInfo, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) Peers(context.Context) ([]*p2p.PeerInfo, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) AddPeer(context.Context, *remoteproto.AddPeerRequest) (*remoteproto.AddPeerReply, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) RemovePeer(context.Context, *remoteproto.RemovePeerRequest) (*remoteproto.RemovePeerReply, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) AddTrustedPeer(context.Context, *remoteproto.AddPeerRequest) (*remoteproto.AddPeerReply, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) RemoveTrustedPeer(context.Context, *remoteproto.RemovePeerRequest) (*remoteproto.RemovePeerReply, error) {
	return nil, errNotServedSnapshotOnly
}

func (b *snapshotOnlyBackend) SetHead(context.Context, *remoteproto.SetHeadRequest) (*remoteproto.SetHeadReply, error) {
	return nil, errNotServedSnapshotOnly
}

// emptyTxPool answers the pool lookups that hash-based transaction methods fall back to when a
// hash is not in the block files. Every other pool method is unreachable through the allowlist.
type emptyTxPool struct {
	txpoolproto.TxpoolClient
}

func (emptyTxPool) Transactions(_ context.Context, in *txpoolproto.TransactionsRequest, _ ...grpc.CallOption) (*txpoolproto.TransactionsReply, error) {
	return &txpoolproto.TransactionsReply{RlpTxs: make([][]byte, len(in.Hashes))}, nil
}
