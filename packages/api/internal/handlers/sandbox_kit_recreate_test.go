package handlers

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
)

func TestKitRecreateRequiresConfiguredInstanceAndPropagatesResumeFailure(t *testing.T) {
	a := &APIStore{}
	team := uuid.New()
	upstream := &api.APIError{Code: http.StatusConflict, ClientMsg: "snapshot changed"}
	failed := func(context.Context) (orchestrator.SandboxMetadata, *api.APIError) {
		return orchestrator.SandboxMetadata{}, upstream
	}
	_, apiErr := a.buildKitRecreateData("instance", team, uuid.Nil, "template", failed)(t.Context())
	require.Same(t, upstream, apiErr)
	resume := func(context.Context) (orchestrator.SandboxMetadata, *api.APIError) {
		return orchestrator.SandboxMetadata{}, nil
	}
	t.Setenv(kitstorage.Environment, "")
	_, apiErr = a.buildKitRecreateData("instance", team, uuid.Nil, "template", resume)(t.Context())
	require.Equal(t, http.StatusBadRequest, apiErr.Code)
	root := filepath.Join(t.TempDir(), "storage")
	t.Setenv(kitstorage.Environment, root)
	store, err := kitstorage.Open(root)
	require.NoError(t, err)
	_, lease, err := store.Prepare(t.Context(), uuid.NewString(), "instance", nil, 0, 0)
	require.NoError(t, err)
	defer lease.Release()
	// Another team's record must not allow access to replacement-template lookup.
	_, apiErr = a.buildKitRecreateData("instance", team, uuid.Nil, "template", resume)(t.Context())
	require.Equal(t, http.StatusBadRequest, apiErr.Code)
	require.Contains(t, apiErr.ClientMsg, "Only native Kit instances")
}
