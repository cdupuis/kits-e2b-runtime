package orchestrator

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/e2b-dev/infra/packages/api/internal/orchestrator/placement"
	"github.com/e2b-dev/infra/packages/shared/pkg/apierrors"
)

func TestKitRecreationRefusalStatus(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"incompatible replacement", createRefusedStatus(t, codes.InvalidArgument, "incompatible retained volume"), http.StatusBadRequest},
		{"untyped error", status.Error(codes.InvalidArgument, "invalid volume"), http.StatusInternalServerError},
		{"worker failure", createRefusedStatusWith(t, codes.Internal, "worker failed", http.StatusInternalServerError), http.StatusInternalServerError},
		{"inconsistent detail", createRefusedStatus(t, codes.Internal, "worker failed"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := placement.SandboxCreateError{Attempts: 1, LastErr: tc.err}
			result := kitRecreatePlacementAPIError(err)
			require.Equal(t, tc.want, result.Code)
			require.Equal(t, apierrors.NoRetry, result.Retryable)
			require.Equal(t, http.StatusInternalServerError, placementAPIError(err).Code)
		})
	}
}
