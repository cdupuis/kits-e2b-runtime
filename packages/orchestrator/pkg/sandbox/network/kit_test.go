//go:build linux

package network

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
)

func TestKitFirewallClosesDNSAndPlatformExceptions(t *testing.T) { //nolint:paralleltest // creates scratch network namespaces
	if os.Geteuid() != 0 {
		t.Skip("requires root for netns + nftables")
	}
	config, err := ParseConfig()
	require.NoError(t, err)
	idx := reserveNSTestIdx(t)
	h := newUDPHarness(t, idx)
	fw, err := NewFirewall(h.fwNSPath(), h.tap, config.OrchestratorInSandboxIPAddress, []string{harnessPlatformIP + "/32"}, nil, NonDNSUDPModeOff)
	require.NoError(t, err)
	slot := &Slot{Idx: idx, Firewall: fw, config: config}
	t.Cleanup(func() { _ = slot.CloseFirewall(context.WithoutCancel(t.Context())) })
	require.NoError(t, slot.UpdateInternet(t.Context(), &orchestrator.SandboxNetworkEgressConfig{KitPolicy: true, DeniedCidrs: []string{"0.0.0.0/0", "::/0"}}))
	dns := listenUDP(t, h, harnessPublicIP, "53")
	tenant := listenUDP(t, h, harnessTenantIP, "443")
	platform := listenUDP(t, h, harnessPlatformIP, "443")
	var tcp net.Listener
	h.in(t, h.sinkNS, func() error {
		var lc net.ListenConfig
		tcp, err = lc.Listen(t.Context(), "tcp4", net.JoinHostPort(harnessPublicIP, "443"))

		return err
	})
	defer tcp.Close()
	sendSideTraffic(t, h)
	require.Zero(t, countDatagrams(t, dns), "DNS cannot bypass Kit declarations")
	require.Zero(t, countDatagrams(t, tenant), "UDP is closed")
	require.Zero(t, countDatagrams(t, platform), "platform allow entries cannot bypass Kit declarations")
	summary := summarizeFilterChain(t, h.fwNS)
	require.True(t, summary.dropIsLast)
	require.False(t, summary.hasAllowSetAccept)
	require.False(t, summary.hasPlainAccept)
	// Reset must restore ordinary E2B behavior for the next tenant.
	require.NoError(t, slot.ResetInternet(t.Context()))
	require.False(t, summarizeFilterChain(t, h.fwNS).hasUnconditionalDrop)
}

func TestKitStorageFirewallIsNarrow(t *testing.T) { //nolint:paralleltest // scratch namespaces
	if os.Geteuid() != 0 {
		t.Skip("requires root for netns + nftables")
	}
	idx := reserveNSTestIdx(t)
	h := newUDPHarness(t, idx)
	fw, err := NewFirewall(h.fwNSPath(), h.tap, harnessPlatformIP, nil, nil, NonDNSUDPModeOff)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fw.Close(context.WithoutCancel(t.Context())) })
	require.NoError(t, fw.ApplyRules(t.Context(), false, NonDNSUDPModeOff, nil, nil, true))
	var listeners []net.Listener
	for _, port := range []string{"111", "2049", "443"} {
		h.in(t, h.sinkNS, func() error {
			var lc net.ListenConfig
			l, err := lc.Listen(t.Context(), "tcp4", net.JoinHostPort(harnessPlatformIP, port))
			if err == nil {
				listeners = append(listeners, l)
			}

			return err
		})
	}
	t.Cleanup(func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	})
	check := func() {
		for _, port := range []string{"111", "2049", "443"} {
			h.in(t, h.guestNS, func() error {
				d := net.Dialer{Timeout: 150 * time.Millisecond}
				c, err := d.DialContext(t.Context(), "tcp4", net.JoinHostPort(harnessPlatformIP, port))
				if port == "443" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					_ = c.Close()
				}

				return nil
			})
		}
	}
	check()
	require.NoError(t, fw.DenyEgress(t.Context()))
	check() // NFS must drain during filesystem checkpoint; other egress stays shut.
}
