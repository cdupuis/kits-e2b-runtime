package kitports

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func tcpServer(t *testing.T) (int, <-chan string) {
	t.Helper()
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	got := make(chan string, 8)
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				b, _ := io.ReadAll(c)
				got <- string(b)
				_, _ = c.Write(append([]byte("reply:"), b...))
			}()
		}
	}()

	return listener.Addr().(*net.TCPAddr).Port, got
}

func activate(t *testing.T, p *Publisher) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ports")
	team, lifecycle := uuid.NewString(), uuid.NewString()
	require.NoError(t, p.Activate(root, team, "instance", lifecycle))
	t.Cleanup(func() { require.NoError(t, p.Close()) })

	return filepath.Join(root, team, "instance", lifecycle+".json")
}

func TestTCPForwardingHalfCloseAndCleanup(t *testing.T) {
	t.Parallel()
	port, got := tcpServer(t)
	createCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, err := Bind(createCtx, netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: port, Name: "app"}})
	require.NoError(t, err)
	record := activate(t, p)
	cancel() // The create request ends while the sandbox and its ports stay live.
	endpoint := p.Endpoints()[0]
	require.Equal(t, "app", endpoint.Name)
	require.Equal(t, "ipv4", endpoint.Family)
	require.Equal(t, "127.0.0.1", netip.MustParseAddrPort(endpoint.Address).Addr().String())
	c, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp4", endpoint.Address)
	require.NoError(t, err)
	require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
	_, err = c.Write([]byte("arbitrary bytes\x00\xff"))
	require.NoError(t, err)
	require.NoError(t, c.(*net.TCPConn).CloseWrite())
	reply, err := io.ReadAll(c)
	require.NoError(t, err)
	require.Equal(t, "reply:arbitrary bytes\x00\xff", string(reply))
	_ = c.Close()
	require.Equal(t, "arbitrary bytes\x00\xff", <-got)
	// A held-open stream must close too, before a network slot can be reused.
	held, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp4", endpoint.Address)
	require.NoError(t, err)
	defer held.Close()
	require.NoError(t, held.SetDeadline(time.Now().Add(time.Second)))
	_, err = held.Write([]byte("pending"))
	require.NoError(t, err)
	require.NoError(t, p.Close())
	_, err = io.ReadAll(held)
	if err != nil {
		var target net.Error
		require.ErrorAs(t, err, &target)
		require.False(t, target.Timeout())
	}
	_, err = os.Stat(record)
	require.True(t, os.IsNotExist(err))
	c, err = (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp4", endpoint.Address)
	if c != nil {
		_ = c.Close()
	}
	require.Error(t, err)
	require.NoError(t, p.Close())
	require.ErrorIs(t, p.Activate(filepath.Dir(record), uuid.NewString(), "again", uuid.NewString()), net.ErrClosed)
}

func TestUDPIsolatesClientsAndPreservesDatagrams(t *testing.T) {
	t.Parallel()
	var lc net.ListenConfig
	server, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer server.Close()
	go func() {
		b := make([]byte, 65535)
		for {
			n, a, err := server.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = server.WriteTo(b[:n], a)
		}
	}()
	port := server.LocalAddr().(*net.UDPAddr).Port
	p, err := Bind(t.Context(), netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: port, Transport: "udp", Family: "dual"}})
	require.NoError(t, err)
	activate(t, p)
	clients := make([]net.Conn, 2)
	for i := range clients {
		clients[i], err = (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "udp4", p.Endpoints()[0].Address)
		require.NoError(t, err)
		t.Cleanup(func() { _ = clients[i].Close() })
		require.NoError(t, clients[i].SetDeadline(time.Now().Add(time.Second)))
	}
	for round := range 3 {
		for i, c := range clients {
			_, err := c.Write([]byte(strconv.Itoa(i) + ":" + strconv.Itoa(round)))
			require.NoError(t, err)
		}
		for i, c := range clients {
			b := make([]byte, 20)
			n, err := c.Read(b)
			require.NoError(t, err)
			require.Equal(t, strconv.Itoa(i)+":"+strconv.Itoa(round), string(b[:n]))
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("UDP shutdown did not close active sessions promptly")
	}
	p.mu.Lock()
	require.Empty(t, p.connections)
	p.mu.Unlock()
}

func TestRegistryLockAndAtomicPublication(t *testing.T) {
	t.Parallel()
	p, err := Bind(t.Context(), netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: 1234}})
	require.NoError(t, err)
	path := activate(t, p)
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	require.ErrorIs(t, unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB), unix.EWOULDBLOCK)
	var r Record
	require.NoError(t, json.NewDecoder(f).Decode(&r))
	require.Equal(t, p.Endpoints(), r.Endpoints)
	// Even a still-open fd becomes distinguishable as stale after close/crash.
	require.NoError(t, p.Close())
	require.NoError(t, unix.Flock(int(f.Fd()), unix.LOCK_SH|unix.LOCK_NB))
}

func TestAdmissionAndUnpublishedClose(t *testing.T) {
	t.Parallel()
	for _, port := range []spec.Port{{Container: 0}, {Container: 70000}, {Container: 80, Transport: "sctp"}, {Container: 80, Family: "ipv6"}} {
		_, err := Bind(t.Context(), netip.MustParseAddr("127.0.0.1"), []spec.Port{port})
		require.Error(t, err)
	}
	_, err := Bind(t.Context(), netip.IPv6Loopback(), []spec.Port{{Container: 80}})
	require.Error(t, err)
	_, err = Bind(t.Context(), netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: 80}, {Container: 80}})
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Bind(ctx, netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: 80}})
	require.ErrorIs(t, err, context.Canceled)
	p, err := Bind(t.Context(), netip.MustParseAddr("127.0.0.1"), []spec.Port{{Container: 80}})
	require.NoError(t, err)
	require.Error(t, p.Activate("relative", uuid.NewString(), "a", uuid.NewString()))
	require.NoError(t, p.Close())
}
