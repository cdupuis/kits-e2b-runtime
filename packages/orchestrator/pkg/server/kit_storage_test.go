package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestClientCannotChooseNativeStorageExports(t *testing.T) {
	t.Parallel()
	_, err := createVolumeMountModelsFromAPI([]*orchestrator.SandboxVolumeMount{{Id: uuid.NewString(), Type: kitstorage.VolumeType, Path: "/steal", Name: "arbitrary"}})
	require.ErrorContains(t, err, "cannot be selected")
}

func TestNativeKitFailedCreateStorageRollback(t *testing.T) {
	t.Parallel()
	for _, snapshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh create", true: "resume"}[snapshot], func(t *testing.T) {
			t.Parallel()
			store, err := kitstorage.Open(filepath.Join(t.TempDir(), "storage"))
			require.NoError(t, err)
			team, instance := uuid.NewString(), "failed-start"
			mounts, lease, err := store.Prepare(t.Context(), team, instance, []kitstorage.Request{
				{Volume: spec.Volume{Path: "/private"}},
				{Volume: spec.Volume{Path: "/shared"}, Kit: "docker.io/test/kit"},
			}, uint32(os.Getuid()), uint32(os.Getgid()))
			require.NoError(t, err)
			private, shared := mounts[0].Directory, mounts[1].Directory
			marker := filepath.Join(private, "state")
			require.NoError(t, os.WriteFile(marker, []byte("preserved"), 0o600))
			rollback := sandbox.NewCleanup()
			releaseCreate := addKitStorageRollback(t.Context(), rollback, store, lease, team, instance, snapshot)
			defer releaseCreate()
			releaseVM := lease.Retain()
			rollback.Add(t.Context(), func(context.Context) error {
				require.FileExists(t, marker, "private data must outlive VM teardown")
				releaseVM()

				return nil
			})
			// Exercise inline rollback before Create's deferred lease release.
			require.NoError(t, rollback.Run(t.Context()))
			releaseCreate()
			require.DirExists(t, shared)
			if snapshot {
				require.FileExists(t, marker, "failed resume must retain private data")
			} else {
				require.NoDirExists(t, private)
				files, err := filepath.Glob(filepath.Join(store.Root, "instances", "*.json"))
				require.NoError(t, err)
				require.Empty(t, files)
			}
		})
	}
}
