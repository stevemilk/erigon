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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/erigontech/erigon/common/log/v3"
	"github.com/erigontech/erigon/db/datadir"
	"github.com/erigontech/erigon/db/snapcfg"
	chainspec "github.com/erigontech/erigon/execution/chain/spec"
)

// TestEraImportMatchesMigration builds segments from archive files and checks them against segments
// the migration built from the decimal ones for the same range. The two paths share nothing but the
// compressor: one re-tiles words copied out of existing segments, the other rebuilds them from the
// archive, recovering every sender and continuing the transaction count itself. Identical bytes mean
// an imported range is interchangeable with a produced one.
//
// Set ERA_DIR to a run of archive files starting at epoch 0, ERA_CHAIN to their chain, and ERA_REF
// to a snapshots directory holding migration-built epoch segments over the same range.
func TestEraImportMatchesMigration(t *testing.T) {
	eraDir, ref := os.Getenv("ERA_DIR"), os.Getenv("ERA_REF")
	if eraDir == "" || ref == "" {
		t.Skip("set ERA_DIR and ERA_REF to run")
	}
	spec, err := chainspec.ChainSpecByName(os.Getenv("ERA_CHAIN"))
	require.NoError(t, err, "set ERA_CHAIN to the chain the archive belongs to")
	snCfg, _ := snapcfg.KnownCfg(spec.Config.ChainName)

	dirs := datadir.New(t.TempDir())
	// Index building needs the salt, and taking the reference's makes the indexes comparable too.
	salt, err := os.ReadFile(filepath.Join(ref, "salt-blocks.txt"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dirs.Snap, "salt-blocks.txt"), salt, 0o644))

	require.NoError(t, ImportEra(context.Background(), eraDir, dirs, spec.Config, snCfg, spec.GenesisHash, 1, log.New()))

	built, err := filepath.Glob(filepath.Join(dirs.Snap, "*-ep.seg"))
	require.NoError(t, err)
	require.NotEmpty(t, built, "the import produced no segments")

	var compared int
	for _, path := range built {
		// The reference may be missing a segment that pruning removed, which is not a mismatch.
		want, err := os.ReadFile(filepath.Join(ref, filepath.Base(path)))
		if os.IsNotExist(err) {
			continue
		}
		require.NoError(t, err)
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Truef(t, bytes.Equal(want, got), "%s differs: %d bytes from the migration, %d from the archive", filepath.Base(path), len(want), len(got))
		compared++
	}
	require.NotZero(t, compared, "no segment name appeared on both sides, so nothing was compared")
	t.Logf("%d segments identical to the migration output", compared)
}
