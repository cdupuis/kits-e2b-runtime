package kit

import (
	"context"
	"errors"
	"testing"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/kits-e2b/guest"
	"github.com/stretchr/testify/require"
)

type lifecycleRecorder struct {
	events []string
	fail   string
}

func (r *lifecycleRecorder) event(name string) error {
	r.events = append(r.events, name)
	if name == r.fail {
		return errors.New("injected failure")
	}

	return nil
}

func (r *lifecycleRecorder) Phase(_ context.Context, p e2b.EgressCandidate) error {
	return r.event(p.AllowOut[0])
}

func (r *lifecycleRecorder) Execute(_ context.Context, p guest.Request) error {
	return r.event(p.Argv[0])
}
func (r *lifecycleRecorder) ColdBoot(context.Context) error { return r.event("cold-boot") }
func (r *lifecycleRecorder) Workload(context.Context, guest.Request) error {
	return r.event("workload")
}
func (r *lifecycleRecorder) RecordInstalled() error { return r.event("record") }

func TestLifecycleBoundaryAndResume(t *testing.T) {
	t.Parallel()
	plan := &e2b.ExecutionPlan{Network: e2b.NetworkCandidate{Install: e2b.EgressCandidate{AllowOut: []string{"install-network"}}, Runtime: e2b.EgressCandidate{AllowOut: []string{"runtime-network"}}}, Install: []guest.Request{{Argv: []string{"install"}}}, Startup: []guest.Request{{Argv: []string{"startup"}}}}
	for _, tc := range []struct {
		name          string
		installed, fs bool
		want          []string
	}{
		{"new", false, false, []string{"install-network", "install", "cold-boot", "runtime-network", "startup", "workload", "record"}},
		{"memory-resume", true, false, nil},
		{"filesystem-resume", true, true, []string{"runtime-network", "startup", "workload", "record"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := new(lifecycleRecorder)
			require.NoError(t, Run(t.Context(), d, plan, tc.installed, tc.fs))
			require.Equal(t, tc.want, d.events)
		})
	}
	// Every failure stops before any later grant or workload can be exposed.
	events := []string{"install-network", "install", "cold-boot", "runtime-network", "startup", "workload", "record"}
	for i, event := range events {
		t.Run("failure-"+event, func(t *testing.T) {
			t.Parallel()
			d := &lifecycleRecorder{fail: event}
			require.Error(t, Run(t.Context(), d, plan, false, false))
			require.Equal(t, events[:i+1], d.events)
		})
	}
}

func TestStorageBeforeHooksAndAfterColdBoot(t *testing.T) {
	t.Parallel()
	plan := &e2b.ExecutionPlan{
		Network: e2b.NetworkCandidate{Install: e2b.EgressCandidate{AllowOut: []string{"install-network"}}, Runtime: e2b.EgressCandidate{AllowOut: []string{"runtime-network"}}},
		Mounts:  []guest.Request{{Argv: []string{"mount"}}},
		Install: []guest.Request{{Argv: []string{"install"}}},
		Startup: []guest.Request{{Argv: []string{"startup"}}},
	}
	d := new(lifecycleRecorder)
	require.NoError(t, Run(t.Context(), d, plan, false, false))
	require.Equal(t, []string{"mount", "install-network", "install", "cold-boot", "mount", "runtime-network", "startup", "workload", "record"}, d.events)
	d = &lifecycleRecorder{fail: "mount"}
	require.ErrorContains(t, Run(t.Context(), d, plan, false, false), "storage")
	require.Equal(t, []string{"mount"}, d.events)
	d = new(lifecycleRecorder)
	require.NoError(t, Run(t.Context(), d, plan, true, true))
	require.Equal(t, []string{"mount", "runtime-network", "startup", "workload", "record"}, d.events)
	d = new(lifecycleRecorder)
	require.NoError(t, Run(t.Context(), d, plan, true, false))
	require.Empty(t, d.events)
}
