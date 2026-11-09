package kit

import (
	"context"
	"fmt"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/kits-e2b/guest"
)

// InstanceDriver owns an unroutable VM. ColdBoot must discard every process,
// socket and kernel state while preserving this instance's filesystem writes.
// A failed operation leaves cleanup to the create transaction; Publish is
// called only after Run returns nil.
type InstanceDriver interface {
	Phase(ctx context.Context, rules e2b.EgressCandidate) error
	Execute(ctx context.Context, request guest.Request) error
	ColdBoot(ctx context.Context) error
	Workload(ctx context.Context, request guest.Request) error
	RecordInstalled() error
}

func Run(ctx context.Context, driver InstanceDriver, plan *e2b.ExecutionPlan, installed, filesystemBoot bool) error {
	if installed && !filesystemBoot {
		return nil
	}
	mount := func() error {
		for _, request := range plan.Mounts {
			if err := driver.Execute(ctx, request); err != nil {
				return fmt.Errorf("kit storage: %w", err)
			}
		}

		return nil
	}
	if err := mount(); err != nil {
		return err
	}
	if !installed && len(plan.Install) != 0 {
		if err := driver.Phase(ctx, plan.Network.Install); err != nil {
			return err
		}
		for _, request := range plan.Install {
			if err := driver.Execute(ctx, request); err != nil {
				return fmt.Errorf("kit install: %w", err)
			}
		}
		if err := driver.ColdBoot(ctx); err != nil {
			return fmt.Errorf("kit phase boundary: %w", err)
		}
		if err := mount(); err != nil {
			return err
		}
	}
	if err := driver.Phase(ctx, plan.Network.Runtime); err != nil {
		return err
	}
	for _, request := range plan.Startup {
		if err := driver.Execute(ctx, request); err != nil {
			return fmt.Errorf("kit startup: %w", err)
		}
	}
	if err := driver.Workload(ctx, plan.Interactive); err != nil {
		return fmt.Errorf("kit workload: %w", err)
	}

	return driver.RecordInstalled()
}
