//go:build linux

package server

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
)

// StopSandboxes runs after RPC admission has ended, while networking, NFS,
// templates and devices are still available. Forced shutdown skips the drain,
// but must still stop every guest and join its lifecycle cleanup.
func (s *Server) StopSandboxes(ctx context.Context) error {
	return stopSandboxLifecycles(ctx, s.sandboxFactory.Sandboxes, func(ctx context.Context, sbx *sandbox.Sandbox) error {
		return sbx.Stop(ctx)
	})
}

func stopSandboxLifecycles(ctx context.Context, sandboxes *sandbox.Map, stop func(context.Context, *sandbox.Sandbox) error) error {
	var group errgroup.Group
	for _, sbx := range sandboxes.LifecycleItems() {
		group.Go(func() error {
			// Preserve an earlier pause/checkpoint reason for lifecycles that
			// are already stopping, and never remove a different execution.
			sbx.SetStopReason(sandbox.StopReasonKilled)
			sandboxes.MarkStopping(ctx, sbx.Runtime.SandboxID, sbx.LifecycleID)
			if err := stop(ctx, sbx); err != nil {
				return fmt.Errorf("stop sandbox %s: %w", sbx.Runtime.SandboxID, err)
			}

			return nil
		})
	}

	stopErr := group.Wait()
	// Stop only ends the processes. Their lifecycle goroutines own Close,
	// including port records, storage leases, NBD and template pins.
	return errors.Join(stopErr, sandboxes.WaitLifecycles(ctx))
}
