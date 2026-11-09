package orchestrator

import (
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/api"
	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/placement"
)

// Recreation carries a replacement composition in the request. A worker's
// explicit invalid-argument refusal is therefore actionable by that caller;
// ordinary creates still treat invalid stored template data as a server error.
func kitRecreatePlacementAPIError(err error) *api.APIError {
	result := placementAPIError(err)
	if result.ErrorCode != errCodeSandboxCreateRefused {
		return result
	}

	var createErr placement.SandboxCreateError
	if !errors.As(err, &createErr) {
		return result
	}
	st, ok := status.FromError(createErr.LastErr)
	if !ok || st.Code() != codes.InvalidArgument {
		return result
	}
	if refusal := createRefusal(st); refusal != nil && refusal.GetHttpStatus() == http.StatusBadRequest {
		result.Code = http.StatusBadRequest
	}

	return result
}
