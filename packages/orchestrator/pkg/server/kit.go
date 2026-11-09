//go:build linux

package server

import (
	"context"
	"fmt"
	"maps"
	"net"
	"strings"
	"sync"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/kits-e2b/guest"
	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/google/uuid"

	kitnative "github.com/e2b-dev/infra/packages/orchestrator/pkg/kit"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/orchestrator"
	"github.com/e2b-dev/infra/packages/shared/pkg/storage"
)

// Register before the VM's rollback so removal runs after VM teardown releases
// its lease. A failed resume keeps the original instance's resumable storage.
func addKitStorageRollback(ctx context.Context, rollback *sandbox.Cleanup, store *kitstorage.Store, lease *kitstorage.Lease, team, instance string, snapshot bool) func() {
	release := sync.OnceFunc(lease.Release)
	if !snapshot {
		rollback.Add(ctx, func(ctx context.Context) error {
			// Some constructor refusals run rollback before Create's defers.
			release()
			_, err := store.Remove(ctx, team, instance)

			return err
		})
	}

	return release
}

func kitEgress(candidate e2b.EgressCandidate) *orchestrator.SandboxNetworkEgressConfig {
	rules := make(map[string]*orchestrator.SandboxNetworkDomainRules)
	for host, entries := range candidate.Rules {
		domain := &orchestrator.SandboxNetworkDomainRules{}
		for _, entry := range entries {
			domain.Rules = append(domain.Rules, &orchestrator.SandboxNetworkRule{Transform: &orchestrator.SandboxNetworkTransform{Headers: maps.Clone(entry.Transform.Headers)}})
		}
		rules[host] = domain
	}

	return &orchestrator.SandboxNetworkEgressConfig{KitPolicy: true, DeniedCidrs: []string{"0.0.0.0/0", "::/0"}, AllowedDomains: candidate.AllowOut, Rules: rules}
}

func (s *Server) kitPhase(ctx context.Context, sbx *sandbox.Sandbox, bundle *e2b.Bundle, rules e2b.EgressCandidate) error {
	var hosts strings.Builder
	hosts.WriteString("127.0.0.1 localhost\n::1 localhost ip6-localhost\n")
	for _, host := range rules.AllowOut {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil {
			return fmt.Errorf("resolve Kit destination %s: %w", host, err)
		}
		if len(ips) == 0 {
			return fmt.Errorf("kit destination %s has no IPv4 address", host)
		}
		for _, ip := range ips {
			if ip.To4() == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				return fmt.Errorf("kit destination %s resolves outside public IPv4", host)
			}
			hosts.WriteString(ip.String() + " " + host + "\n")
		}
	}
	id, err := guest.ResolveIdentity(bundle.Passwd, bundle.Group, "0")
	if err != nil {
		return err
	}
	// resolv.conf is made immutable by E2B provisioning. Kernel policy closes
	// DNS regardless; /etc/hosts supplies only the declared destinations.
	if err := sbx.RunKitRequest(ctx, guest.Request{Action: "write", Identity: id, File: &guest.File{Path: "/etc/hosts", Content: hosts.String(), Mode: 0o644, Overwrite: true}}, false); err != nil {
		return err
	}
	egress := kitEgress(rules)

	return s.sandboxFactory.RunUpdate(ctx, sbx, egress, func() error {
		return transitionEgress(ctx, sbx.Config, sbx.Slot.UpdateInternet, sbx.Config.GetNetworkEgress(), egress)
	})
}

// initializeKit owns the phase boundary before this instance becomes routable.
// Install writes survive in a private filesystem snapshot; all install
// processes, sockets and kernel state are discarded through a fresh VM boot.
func (s *Server) initializeKit(ctx context.Context, sbx **sandbox.Sandbox, meta metadata.Template, plan *e2b.ExecutionPlan, filesystemBoot bool, release *func()) error {
	return kitnative.Run(ctx, &nativeKitDriver{server: s, sbx: sbx, meta: meta, release: release}, plan, meta.KitInstalled, filesystemBoot)
}

type nativeKitDriver struct {
	server  *Server
	sbx     **sandbox.Sandbox
	meta    metadata.Template
	release *func()
}

func (d *nativeKitDriver) Phase(ctx context.Context, rules e2b.EgressCandidate) error {
	return d.server.kitPhase(ctx, *d.sbx, d.meta.Kit, rules)
}

func (d *nativeKitDriver) Execute(ctx context.Context, request guest.Request) error {
	return (*d.sbx).RunKitRequest(ctx, request, false)
}

func (d *nativeKitDriver) Workload(ctx context.Context, request guest.Request) error {
	return (*d.sbx).RunKitRequest(ctx, request, true)
}

func (d *nativeKitDriver) ColdBoot(ctx context.Context) error {
	s, current, meta := d.server, *d.sbx, d.meta

	if err := current.Slot.DenyEgress(ctx); err != nil {
		return err
	}
	// Mark only this instance's metadata. Never mutate the reusable cache entry.
	meta.KitInstalled = true
	meta.KitInstanceID = current.Runtime.SandboxID
	current.Template = sbxtemplate.NewMaskTemplate(current.Template, sbxtemplate.WithMetadata(meta))
	buildID := uuid.NewString()
	snapshot, err := s.snapshotAndCacheSandbox(ctx, current, buildID, nil, storage.ObjectOriginPause, true, false, false)
	if err != nil {
		return err
	}
	// The upload owns its resources and is drained on orchestrator shutdown.
	s.uploadSnapshotAsync(ctx, current, snapshot)
	installed, releaseInstalled, err := s.templateCache.GetTemplatePinned(ctx, buildID, true, false, sbxtemplate.GetTemplateOpts{MaxSandboxLengthHours: current.Config.MaxSandboxLengthHours})
	if err != nil {
		return err
	}
	oldRelease := *d.release
	*d.release = func() { oldRelease(); releaseInstalled() }
	if err := stopAndCloseSandbox(ctx, current); err != nil {
		return err
	}
	current.Config.SetNetworkEgress(kitEgress(e2b.EgressCandidate{}))
	rebooted, err := s.sandboxFactory.RebootSandbox(ctx, installed, current.Config, current.Runtime, current.GetEndAt(), current.APIStoredConfig, true, false, nil)
	if err != nil {
		return err
	}
	*d.sbx = rebooted

	return nil
}

func (d *nativeKitDriver) RecordInstalled() error {
	current := *d.sbx

	// Record on the running handle so every snapshot path inherits it. Re-read
	// after reboot to retain its filesystem/CPU/device metadata.
	actual, err := current.Template.Metadata()
	if err != nil {
		return err
	}
	actual.KitInstalled = true
	actual.KitInstanceID = current.Runtime.SandboxID
	current.Template = sbxtemplate.NewMaskTemplate(current.Template, sbxtemplate.WithMetadata(actual))

	return nil
}

func hasKitNetworkOverride(egress *orchestrator.SandboxNetworkEgressConfig) bool {
	return len(egress.GetAllowedCidrs()) != 0 || len(egress.GetDeniedCidrs()) != 0 || len(egress.GetAllowedDomains()) != 0 || len(egress.GetRules()) != 0 || egress.GetEgressProxyAddress() != ""
}
