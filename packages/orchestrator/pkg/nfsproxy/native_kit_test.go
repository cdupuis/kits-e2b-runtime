//go:build linux

package nfsproxy

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/chrooted"
	nfscfg "github.com/e2b-dev/infra/packages/orchestrator/pkg/nfsproxy/cfg"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
)

// Exercise allocator output against the actual NFS server and host filesystem.
func TestNativeKitStorageRoundTrip(t *testing.T) {
	t.Parallel()
	if os.Geteuid() != 0 {
		t.Skip("NFS chroot filesystem requires root")
	}
	team := uuid.New()
	store, err := kitstorage.Open(filepath.Join(t.TempDir(), "kits"))
	require.NoError(t, err)
	mounts, lease, err := store.Prepare(t.Context(), team.String(), "first", []kitstorage.Request{
		{Volume: spec.Volume{Path: "/private"}},
		{Kit: "docker.io/org/kit", Volume: spec.Volume{Path: "/shared"}},
	}, 1000, 1000)
	require.NoError(t, err)
	defer lease.Release()
	var exports []sandbox.VolumeMountConfig
	for _, m := range mounts {
		exports = append(exports, sandbox.VolumeMountConfig{ID: uuid.MustParse(m.ID), Name: "kit-" + m.ID, Path: m.Volume.Path, Type: kitstorage.VolumeType})
	}
	sandboxes := sandbox.NewSandboxesMap()
	sbx := &sandbox.Sandbox{LifecycleID: uuid.NewString(), Metadata: &sandbox.Metadata{
		Config:  sandbox.NewConfig(sandbox.Config{VolumeMounts: exports}),
		Runtime: sandboxtypes.RuntimeMetadata{SandboxID: "first", TeamID: team.String()},
	}, Resources: &sandbox.Resources{Slot: &network.Slot{Key: "native-test", HostIP: net.IPv4(127, 0, 0, 1)}}}
	sandboxes.AssignNetwork(t.Context(), sbx)
	builder := chrooted.NewBuilder(cfg.Config{PersistentVolumeMounts: map[string]string{kitstorage.VolumeType: filepath.Join(store.Root, "data")}})
	proxy, err := NewProxy(t.Context(), builder, sandboxes, nfscfg.Config{})
	require.NoError(t, err)
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- proxy.Serve(listener) }()
	defer func() { require.NoError(t, listener.Close()); require.NoError(t, <-done) }()
	client, err := rpc.DialTCP("tcp", listener.Addr().String(), false)
	require.NoError(t, err)
	defer client.Close()
	mount := &nfs.Mount{Client: client}
	auth := rpc.NewAuthUnix("", 1000, 1000)
	target, err := mount.Mount("/"+exports[1].Name, auth.Auth())
	require.NoError(t, err)
	file, err := target.OpenFile("from-guest", 0o600)
	require.NoError(t, err)
	_, err = file.Write([]byte("shared data"))
	require.NoError(t, err)
	require.NoError(t, file.Close())
	data, err := os.ReadFile(filepath.Join(mounts[1].Directory, "from-guest"))
	require.NoError(t, err)
	require.Equal(t, "shared data", string(data))
	// A different instance reattaches the Kit share but never the private volume.
	second, secondLease, err := store.Prepare(t.Context(), team.String(), "second", []kitstorage.Request{mounts[0].Request, mounts[1].Request}, 1000, 1000)
	require.NoError(t, err)
	defer secondLease.Release()
	require.Equal(t, mounts[1].ID, second[1].ID)
	require.NotEqual(t, mounts[0].ID, second[0].ID)
	_, err = mount.Mount("/kit-"+second[0].ID, auth.Auth())
	require.Error(t, err, "an ungranted private volume must be inaccessible")
	_, err = mount.Mount("/../../etc", auth.Auth())
	require.Error(t, err)
}
