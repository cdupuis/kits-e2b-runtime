//go:build linux

package sandbox

import (
	"context"
	"errors"
	"net"
	"net/netip"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/kitports"
)

// PublishNativePorts runs only when promoting a fully initialized instance.
// Build VMs, install VMs and prefetch harvests never get host listeners.
func (s *Sandbox) PublishNativePorts(ctx context.Context, root string) error {
	s.nativePortsMu.Lock()
	defer s.nativePortsMu.Unlock()
	if s.nativePortsClosed {
		return net.ErrClosed
	}
	if s.nativePorts != nil {
		return nil
	}
	if s.Template == nil {
		return nil
	}
	meta, err := s.Template.Metadata()
	if err != nil {
		return err
	}
	if meta.Kit == nil {
		return nil
	}
	ports, err := meta.Kit.NativePortRequests()
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		return nil
	}
	if err := kitports.ValidateDirectory(root); err != nil {
		return err
	}
	if s.Resources == nil || s.Slot == nil {
		return errors.New("native port publication requires a local sandbox slot")
	}
	ip, err := netip.ParseAddr(s.Slot.HostIPString())
	if err != nil {
		return err
	}
	publisher, err := kitports.Bind(ctx, ip, ports)
	if err != nil {
		return err
	}
	if err := publisher.Activate(root, s.Runtime.TeamID, s.Runtime.SandboxID, s.LifecycleID); err != nil {
		return errors.Join(err, publisher.Close())
	}
	s.nativePorts = publisher
	// Priority cleanup precedes slot release even on a failed live registration.
	s.cleanup.AddPriority(ctx, func(context.Context) error { return publisher.Close() })

	return nil
}

func (s *Sandbox) closeNativePorts() error {
	s.nativePortsMu.Lock()
	defer s.nativePortsMu.Unlock()
	s.nativePortsClosed = true
	if s.nativePorts == nil {
		return nil
	}

	return s.nativePorts.Close()
}
