package kitports

import (
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// The smoke script boots a guest echo service on an isolated TAP network and
// runs this binary in that network namespace. No Embed control plane is needed.
func TestFirecrackerPublishedPorts(t *testing.T) {
	t.Parallel()
	target := os.Getenv("KIT_PORT_VM_TARGET")
	if target == "" {
		t.Skip("run through examples/native-kit/firecracker-ports-smoke.sh")
	}
	ip, err := netip.ParseAddr(target)
	require.NoError(t, err)
	p, err := Bind(t.Context(), ip, []spec.Port{{Name: "tcp-echo", Container: 3000}, {Name: "udp-echo", Container: 3001, Transport: "udp", Family: "dual"}})
	require.NoError(t, err)
	defer p.Close()
	require.NoError(t, p.Activate(filepath.Join(t.TempDir(), "ports"), uuid.NewString(), "firecracker", uuid.NewString()))
	for _, endpoint := range p.Endpoints() {
		d := net.Dialer{Timeout: time.Second}
		c, err := d.DialContext(t.Context(), endpoint.Transport+"4", endpoint.Address)
		require.NoError(t, err)
		require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
		payload := []byte("native-ports\x00\xff")
		_, err = c.Write(payload)
		require.NoError(t, err)
		b := make([]byte, 128)
		var n int
		if endpoint.Transport == "tcp" {
			n, err = io.ReadFull(c, b[:len(payload)])
		} else {
			n, err = c.Read(b)
		}
		require.NoError(t, err)
		require.Equal(t, payload, b[:n])
		require.NoError(t, c.Close())
	}
	require.NoError(t, p.Close())
	t.Log("PASS: host-allocated TCP and UDP loopback ports reach the Firecracker guest")
}
