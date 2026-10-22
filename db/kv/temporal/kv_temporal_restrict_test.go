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

package temporal

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/kv"
	"github.com/erigontech/erigon/db/kv/dbcfg"
	"github.com/erigontech/erigon/db/kv/mdbx/mdbxtest"
	"github.com/erigontech/erigon/db/kv/order"
	"github.com/erigontech/erigon/db/state"
)

func TestTemporalTx_RestrictedReadsFail(t *testing.T) {
	t.Parallel()
	ctx := t.Context()

	agg := state.NewTest(datadir.New(t.TempDir())).StepSize(1).MustOpen(ctx)
	defer agg.Close()
	db, err := New(mdbxtest.NewTestDB(t, dbcfg.ChainDB), agg, nil)
	require.NoError(t, err)
	defer db.Close()
	db.Restrict([]kv.Domain{kv.RCacheDomain}, []kv.InvertedIdx{kv.LogAddrIdx})

	tx, err := db.BeginTemporalRo(ctx)
	require.NoError(t, err)
	defer tx.Rollback()

	reads := map[string]func(kv.Domain) error{
		"GetLatest": func(d kv.Domain) error {
			_, _, readErr := tx.GetLatest(d, []byte{1}, kv.GetLatestOptions{})
			return readErr
		},
		"GetLatestValSize": func(d kv.Domain) error {
			_, _, readErr := tx.GetLatestValSize(d, []byte{1})
			return readErr
		},
		"GetAsOf": func(d kv.Domain) error {
			_, _, readErr := tx.GetAsOf(d, []byte{1}, 1)
			return readErr
		},
		"HistorySeek": func(d kv.Domain) error {
			_, _, readErr := tx.HistorySeek(d, []byte{1}, 1)
			return readErr
		},
		"RangeAsOf": func(d kv.Domain) error {
			it, readErr := tx.RangeAsOf(d, nil, nil, 1, order.Asc, kv.Unlim)
			if it != nil {
				it.Close()
			}
			return readErr
		},
		"RangeLatest": func(d kv.Domain) error {
			it, readErr := tx.Debug().RangeLatest(d, nil, nil, kv.Unlim)
			if it != nil {
				it.Close()
			}
			return readErr
		},
		"GetLatestFromDB": func(d kv.Domain) error {
			_, _, _, readErr := tx.Debug().GetLatestFromDB(d, []byte{1})
			return readErr
		},
		"GetLatestFromFiles": func(d kv.Domain) error {
			_, _, _, _, readErr := tx.Debug().GetLatestFromFiles(d, []byte{1}, 1)
			return readErr
		},
		"TraceKey": func(d kv.Domain) error {
			it, readErr := tx.Debug().TraceKey(d, []byte{1}, 0, 1)
			if it != nil {
				it.Close()
			}
			return readErr
		},
		"HistoryKeyTxNumRange": func(d kv.Domain) error {
			it, readErr := tx.Debug().HistoryKeyTxNumRange(d, 0, 1, order.Asc, kv.Unlim)
			if it != nil {
				it.Close()
			}
			return readErr
		},
		"HistoryRange": func(d kv.Domain) error {
			it, readErr := tx.HistoryRange(d, 0, 1, order.Asc, kv.Unlim)
			if it != nil {
				it.Close()
			}
			return readErr
		},
	}
	for name, read := range reads {
		require.ErrorIs(t, read(kv.AccountsDomain), ErrDenied, name)
		require.NoError(t, read(kv.RCacheDomain), name)
	}

	_, err = tx.IndexRange(kv.TracesFromIdx, []byte{1}, 0, 1, order.Asc, kv.Unlim)
	require.ErrorIs(t, err, ErrDenied)
	it, err := tx.IndexRange(kv.LogAddrIdx, []byte{1}, 0, 1, order.Asc, kv.Unlim)
	require.NoError(t, err)
	it.Close()
}
