//go:build linux

package cfg

import (
	"path/filepath"
	"testing"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/kitports"
)

func TestNativeStorageConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "kits")
	t.Setenv(kitstorage.Environment, root)
	t.Setenv("PERSISTENT_VOLUME_MOUNTS", "")
	config, err := Parse()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "data"), config.PersistentVolumeMounts[kitstorage.VolumeType])
	t.Setenv("PERSISTENT_VOLUME_MOUNTS", "native-kit:"+root)
	_, err = Parse()
	require.ErrorContains(t, err, "reserved")
}

func TestNativePortConfiguration(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ports")
	t.Setenv(kitports.Environment, root)
	config, err := Parse()
	require.NoError(t, err)
	require.Equal(t, root, config.NativeKitPortsDir)
	t.Setenv(kitports.Environment, "relative")
	_, err = Parse()
	require.ErrorContains(t, err, "absolute dedicated")
}
