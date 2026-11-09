//go:build linux

package network

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestCreatedNamespaceForwardsIPv4(t *testing.T) {
	if os.Getenv("E2B_NETWORK_NAMESPACE_TESTS") == "" || os.Geteuid() != 0 {
		t.Skip("requires root and E2B_NETWORK_NAMESPACE_TESTS=1")
	}
	slot, err := NewSlot("forwarding-test", 30000, Config{OrchestratorInSandboxIPAddress: "192.0.2.1"}, NewNoopEgressProxy())
	require.NoError(t, err)
	if _, err := os.Stat(filepath.Join(NetNamespacesDir, slot.NamespaceID())); !os.IsNotExist(err) {
		t.Skip("test slot namespace is already in use")
	}
	require.NoError(t, slot.CreateNetwork(t.Context()))
	t.Cleanup(func() { require.NoError(t, slot.RemoveNetwork(t.Context())) })
	out, err := exec.CommandContext(t.Context(), "ip", "netns", "exec", slot.NamespaceID(), "cat", "/proc/sys/net/ipv4/ip_forward").Output()
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(string(out)), "the guest TAP must route through the namespace veth")
}

type fakeNotExistError struct {
	notExist bool
}

func (e fakeNotExistError) Error() string {
	return "fake iptables error"
}

func (e fakeNotExistError) IsNotExist() bool {
	return e.notExist
}

func TestIgnoreExpectedAbsentHandlesWrappedAndJoinedErrors(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("wrapped: %w", fakeNotExistError{notExist: true})
	joined := errors.Join(wrapped, fakeNotExistError{notExist: true})

	require.True(t, ignoreExpectedAbsent(joined, isIPTablesNotExist))
	require.False(t, ignoreExpectedAbsent(errors.Join(joined, errors.New("boom")), isIPTablesNotExist))
	require.False(t, ignoreExpectedAbsent(fakeNotExistError{notExist: false}, isIPTablesNotExist))
}

func TestExpectedAbsentClassifiers(t *testing.T) {
	t.Parallel()

	require.True(t, isRouteNotExist(fmt.Errorf("route delete failed: %w", unix.ESRCH)))
	require.False(t, isRouteNotExist(unix.EPERM))

	require.True(t, isLinkNotExist(fmt.Errorf("link delete failed: %w", unix.ENODEV)))
	require.False(t, isLinkNotExist(unix.EPERM))

	require.True(t, isNamespaceNotExist(&os.PathError{Op: "remove", Path: "/var/run/netns/missing", Err: unix.ENOENT}))
	require.False(t, isNamespaceNotExist(unix.EPERM))
}
