package handlers

import (
	"context"
	"net/http"
	"os"

	kitstorage "github.com/docker/kits-e2b/storage"
	"github.com/google/uuid"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	templatecache "github.com/e2b-dev/infra/packages/api/internal/cache/templates"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator"
)

// The ordinary resume reservation serializes recreation against resume/delete.
// The authenticated paused snapshot supplies the identity and immutable host
// bindings. Only its writable layer and Kit composition are replaced.
func (a *APIStore) buildKitRecreateData(sandboxID string, teamID, clusterID uuid.UUID, templateID string, resume orchestrator.SandboxDataFetcher) orchestrator.SandboxDataFetcher {
	return func(ctx context.Context) (orchestrator.SandboxMetadata, *api.APIError) {
		data, apiErr := resume(ctx)
		if apiErr != nil {
			return data, apiErr
		}
		root := os.Getenv(kitstorage.Environment)
		if root == "" {
			return data, &api.APIError{Code: http.StatusBadRequest, ClientMsg: "Native Kit recreation is not configured"}
		}
		store, err := kitstorage.Open(root)
		if err != nil {
			return data, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Native Kit storage is unavailable", Err: err}
		}
		exists, err := store.HasInstance(ctx, teamID.String(), sandboxID)
		if err != nil {
			return data, &api.APIError{Code: http.StatusInternalServerError, ClientMsg: "Cannot inspect native Kit instance", Err: err}
		}
		if !exists {
			return data, &api.APIError{Code: http.StatusBadRequest, ClientMsg: "Only native Kit instances can be recreated"}
		}
		// A direct template ID keeps lookup and authorization identical to create.
		env, build, err := a.templateCache.Get(ctx, templateID, nil, teamID, clusterID)
		if err != nil {
			return data, (templatecache.TemplateRef{Identifier: templateID}).APIError(err)
		}
		if data.EnvdAccessToken != nil {
			token, tokenErr := a.getEnvdAccessToken(build.EnvdVersion, sandboxID)
			if tokenErr != nil {
				return data, tokenErr
			}
			data.EnvdAccessToken = &token
		}
		data.Build = *build
		data.TemplateID, data.BaseTemplateID = env.TemplateID, env.TemplateID
		data.Alias = firstAlias(env.Aliases)
		data.NativeKitRecreate = true
		data.FilesystemBoot = true
		data.FilesystemOnlySnapshot = build.FilesystemOnly
		// Keep node affinity: private native storage is local Embed storage.
		return data, nil
	}
}
