//go:build linux

package sandbox

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/sandbox-kit-spec/v3/fetch"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/network"
	sbxtemplate "github.com/e2b-dev/infra/packages/orchestrator/pkg/sandbox/template"
	"github.com/e2b-dev/infra/packages/orchestrator/pkg/template/metadata"
	"github.com/e2b-dev/infra/packages/shared/pkg/sandboxtypes"
)

type portTestTemplate struct {
	sbxtemplate.Template

	kit *e2b.Bundle
}

func (p portTestTemplate) Metadata() (metadata.Template, error) {
	return metadata.Template{Kit: p.kit}, nil
}

func TestNativePortsCloseBeforeSlotCleanup(t *testing.T) {
	t.Parallel()
	b := &e2b.Bundle{Plan: &e2b.Plan{Options: e2b.PrepareOptions{NativePorts: true}}}
	b.Plan.Resolved = &fetch.Resolved{Descriptor: &spec.Descriptor{Capabilities: []spec.Capability{{Type: spec.CapabilityPort, Config: map[string]any{"container": 3000, "name": "app"}}}}}
	s := &Sandbox{Template: portTestTemplate{kit: b}, LifecycleID: uuid.NewString(), cleanup: NewCleanup(), Resources: &Resources{Slot: &network.Slot{HostIP: net.IPv4(127, 0, 0, 1)}}, Metadata: &Metadata{Runtime: sandboxtypes.RuntimeMetadata{TeamID: uuid.NewString(), SandboxID: "port-test"}}}
	root := filepath.Join(t.TempDir(), "ports")
	require.NoError(t, s.PublishNativePorts(t.Context(), root))
	endpoint := s.nativePorts.Endpoints()[0].Address
	s.cleanup.Add(t.Context(), func(ctx context.Context) error {
		c, err := (&net.Dialer{Timeout: 100 * time.Millisecond}).DialContext(ctx, "tcp4", endpoint)
		if c != nil {
			_ = c.Close()
		}
		require.Error(t, err, "forwarder survived until slot cleanup")

		return nil
	})
	require.NoError(t, s.Close(t.Context()))
	require.ErrorIs(t, s.PublishNativePorts(t.Context(), root), net.ErrClosed)
	files, err := filepath.Glob(filepath.Join(root, "*", "*", "*.json"))
	require.NoError(t, err)
	require.Empty(t, files)
}
