package orchestrator

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/stretchr/testify/require"
)

func TestExpiredKitStorageFollowsResumableSnapshot(t *testing.T) {
	// The configured store is process-wide; subtests intentionally run serially.
	for _, state := range []string{"no snapshot", "retained snapshot", "lookup failed"} {
		t.Run(state, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "storage")
			t.Setenv(kitstorage.Environment, root)
			o, _, node, sbx := newPauseFixture(t, nil)
			store, err := kitstorage.Open(root)
			require.NoError(t, err)
			mounts, lease, err := store.Prepare(t.Context(), sbx.TeamID.String(), sbx.SandboxID, []kitstorage.Request{
				{Volume: spec.Volume{Path: "/private"}},
			}, uint32(os.Getuid()), uint32(os.Getgid()))
			require.NoError(t, err)
			marker := filepath.Join(mounts[0].Directory, "data")
			require.NoError(t, os.WriteFile(marker, []byte("retained"), 0o600))
			lease.Release()
			ctx := t.Context()
			if state == "retained snapshot" {
				seedFilesystemOnlyBuild(t, o, node, sbx)
			}
			if state == "lookup failed" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err = o.removeExpiredKitStorage(ctx, sbx.ToNodeSandbox())
			if state == "lookup failed" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if state == "no snapshot" {
				_, err := os.Stat(marker)
				require.True(t, os.IsNotExist(err), "an instance with no resumable snapshot should release private data")
			} else {
				data, err := os.ReadFile(marker)
				require.NoError(t, err)
				require.Equal(t, "retained", string(data))
			}
		})
	}
}
