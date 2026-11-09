//go:build linux

package factories

import (
	"context"
	"time"
)

const cleanupTimeout = 30 * time.Second

// Forced stop cancels the drain context to skip waiting for workloads. Resource
// cleanup must still run, with a fresh budget for each service.
func cleanupCloser(name string, closeService func(context.Context) error) closer {
	return closer{name, func(ctx context.Context) error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		return closeService(cleanupCtx)
	}}
}
