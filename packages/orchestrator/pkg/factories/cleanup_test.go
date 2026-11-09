//go:build linux

package factories

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCleanupAfterForcedDrainHasIndependentBudgets(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		type traceKey struct{}
		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), traceKey{}, "shutdown"))
		cancel()
		for range 2 {
			started := time.Now()
			registered := cleanupCloser("stalled resource", func(ctx context.Context) error {
				require.NoError(t, ctx.Err())
				require.Equal(t, "shutdown", ctx.Value(traceKey{}))
				<-ctx.Done()

				return ctx.Err()
			})
			require.ErrorIs(t, registered.close(ctx), context.DeadlineExceeded)
			require.Equal(t, cleanupTimeout, time.Since(started))
		}
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
}

func TestCleanupReleasesContextAndRetainsFailure(t *testing.T) {
	t.Parallel()

	failure := errors.New("resource release failed")
	var calledDone <-chan struct{}
	registered := cleanupCloser("resource", func(ctx context.Context) error {
		calledDone = ctx.Done()
		require.NoError(t, ctx.Err())

		return failure
	})
	require.ErrorIs(t, registered.close(t.Context()), failure)
	select {
	case <-calledDone:
	default:
		t.Fatal("cleanup context was not released")
	}
}
