//go:build linux

package server

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox"
)

func TestStopSandboxesJoinsEveryLifecycleBeforeResourceTeardown(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sandboxes := sandbox.NewSandboxesMap()
		old := drainTestSandbox(t, "old")
		old.SetStopReason(sandbox.StopReasonPaused)
		sandboxes.TrackLifecycle(t.Context(), old)
		current := drainTestSandbox(t, "current")
		require.NoError(t, sandboxes.MarkRunning(t.Context(), current))

		stopped := make(chan *sandbox.Sandbox, 2)
		allowStop := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- stopSandboxLifecycles(t.Context(), sandboxes, func(_ context.Context, sbx *sandbox.Sandbox) error {
				stopped <- sbx
				<-allowStop

				return nil
			})
		}()
		synctest.Wait()
		require.Len(t, stopped, 2, "all guests receive stop before waiting for another to finish")
		require.Zero(t, sandboxes.Count(), "remaining executions are no longer routable")
		require.Equal(t, sandbox.StopReasonPaused, old.GetStopReason())
		require.Equal(t, sandbox.StopReasonKilled, current.GetStopReason())
		close(allowStop)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("teardown continued before lifecycle cleanup: %v", err)
		default:
		}
		sandboxes.MarkStopped(t.Context(), current)
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("old lifecycle cleanup was still outstanding: %v", err)
		default:
		}
		sandboxes.MarkStopped(t.Context(), old)
		require.NoError(t, <-done)
	})
}

func TestStopSandboxesFailureStillStopsPeersAndWaitsForCleanup(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sandboxes := sandbox.NewSandboxesMap()
		for _, id := range []string{"first", "second"} {
			sandboxes.TrackLifecycle(t.Context(), drainTestSandbox(t, id))
		}
		failure := errors.New("guest stop failed")
		stopped := make(chan struct{}, 2)
		done := make(chan error, 1)
		go func() {
			done <- stopSandboxLifecycles(t.Context(), sandboxes, func(_ context.Context, _ *sandbox.Sandbox) error {
				stopped <- struct{}{}

				return failure
			})
		}()
		synctest.Wait()
		require.Len(t, stopped, 2)
		select {
		case err := <-done:
			t.Fatalf("stop failure skipped lifecycle cleanup: %v", err)
		default:
		}
		for _, sbx := range sandboxes.LifecycleItems() {
			sandboxes.MarkStopped(t.Context(), sbx)
		}
		require.ErrorIs(t, <-done, failure)
	})
}

func TestStopSandboxesCleanupBudgetExpires(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		sandboxes := sandbox.NewSandboxesMap()
		sandboxes.TrackLifecycle(t.Context(), drainTestSandbox(t, "stalled"))
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		err := stopSandboxLifecycles(ctx, sandboxes, func(ctx context.Context, _ *sandbox.Sandbox) error {
			<-ctx.Done()

			return ctx.Err()
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Len(t, sandboxes.LifecycleItems(), 1, "timeout must not pretend cleanup finished")
	})
}
