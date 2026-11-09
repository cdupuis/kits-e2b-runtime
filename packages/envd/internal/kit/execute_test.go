//go:build linux

package kit

import (
	"os"
	"strconv"
	"testing"

	"github.com/docker/kits-e2b/guest"
	"github.com/stretchr/testify/require"
)

func TestRefusesNonPrivateRequestPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"/tmp/request.json", "/run/e2b-kit/../request.json", "relative", "/run/e2b-kit/sub/request.json"} {
		_, err := readRequest(name)
		require.Error(t, err)
	}
}

func TestIdentityDriftRefused(t *testing.T) {
	t.Parallel()
	passwd, err := os.ReadFile("/etc/passwd")
	require.NoError(t, err)
	group, err := os.ReadFile("/etc/group")
	require.NoError(t, err)
	id, err := guest.ResolveIdentity(string(passwd), string(group), strconv.Itoa(os.Getuid()))
	require.NoError(t, err)
	require.NoError(t, checkIdentity(id))
	id.Home += "/changed"
	require.ErrorContains(t, checkIdentity(id), "identity changed")
}
