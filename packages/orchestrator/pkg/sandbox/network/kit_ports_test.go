//go:build linux

package network

import (
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/kitports"
)

func TestKitPublishedTCPAndUDPThroughFirewall(t *testing.T) { //nolint:paralleltest // scratch network namespaces
	if os.Geteuid() != 0 {
		t.Skip("requires root for network namespaces")
	}
	h := newUDPHarness(t, reserveNSTestIdx(t))
	ipCmd(t, "-n", h.sinkNS, "link", "set", "lo", "up")
	fw, err := NewFirewall(h.fwNSPath(), h.tap, harnessPlatformIP, nil, nil, NonDNSUDPModeOff)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fw.Close(t.Context()) })
	require.NoError(t, fw.ApplyRules(t.Context(), false, NonDNSUDPModeOff, nil, nil, true))
	var tcp net.Listener
	var udp net.PacketConn
	h.in(t, h.guestNS, func() error {
		var lc net.ListenConfig
		tcp, err = lc.Listen(t.Context(), "tcp4", net.JoinHostPort(harnessGuestIP, "0"))
		if err != nil {
			return err
		}
		udp, err = lc.ListenPacket(t.Context(), "udp4", net.JoinHostPort(harnessGuestIP, "0"))

		return err
	})
	defer tcp.Close()
	defer udp.Close()
	go func() {
		c, err := tcp.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	go func() {
		data := make([]byte, 1024)
		n, a, err := udp.ReadFrom(data)
		if err == nil {
			_, _ = udp.WriteTo(data[:n], a)
		}
	}()
	command := exec.CommandContext(t.Context(), "ip", "netns", "exec", h.sinkNS, os.Args[0], "-test.run=^TestKitPortForwarderHelper$", "-test.timeout=15s")
	command.Env = append(os.Environ(), "KIT_PORT_HELPER=1", "KIT_PORT_TCP="+strconv.Itoa(tcp.Addr().(*net.TCPAddr).Port), "KIT_PORT_UDP="+strconv.Itoa(udp.LocalAddr().(*net.UDPAddr).Port))
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
}

func TestKitPortForwarderHelper(t *testing.T) {
	t.Parallel()
	if os.Getenv("KIT_PORT_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	tcp, err := strconv.Atoi(os.Getenv("KIT_PORT_TCP"))
	require.NoError(t, err)
	udp, err := strconv.Atoi(os.Getenv("KIT_PORT_UDP"))
	require.NoError(t, err)
	p, err := kitports.Bind(t.Context(), netip.MustParseAddr(harnessGuestIP), []spec.Port{{Container: tcp}, {Container: udp, Transport: "udp", Family: "dual"}})
	require.NoError(t, err)
	defer p.Close()
	require.NoError(t, p.Activate(filepath.Join(t.TempDir(), "ports"), uuid.NewString(), "firewall-test", uuid.NewString()))
	for _, endpoint := range p.Endpoints() {
		d := net.Dialer{Timeout: time.Second}
		c, err := d.DialContext(t.Context(), endpoint.Transport+"4", endpoint.Address)
		require.NoError(t, err)
		require.NoError(t, c.SetDeadline(time.Now().Add(3*time.Second)))
		_, err = c.Write([]byte("raw\x00reply"))
		require.NoError(t, err)
		data := make([]byte, 9)
		_, err = io.ReadFull(c, data)
		require.NoError(t, err)
		require.Equal(t, "raw\x00reply", string(data))
		require.NoError(t, c.Close())
	}
}
