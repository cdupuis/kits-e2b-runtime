//go:build linux

package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/docker/kits-e2b/guest"
	"github.com/stretchr/testify/require"

	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process"
	"github.com/e2b-dev/infra/packages/shared/pkg/grpc/envd/process/processconnect"
)

func TestKitWorkloadStartDoesNotTransmitCreateDeadline(t *testing.T) {
	t.Parallel()
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Clone()
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()

	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	client := processconnect.NewProcessClient(server.Client(), server.URL)
	stream, err := client.Start(kitWorkloadContext{parent}, connect.NewRequest(&process.StartRequest{Process: &process.ProcessConfig{Cmd: "shell"}}))
	require.NoError(t, err)
	defer stream.Close()
	require.Empty(t, (<-received).Get("Connect-Timeout-Ms"), "envd must not time out the workload at the create deadline")
	cancel()
	require.False(t, stream.Receive(), "cancellation must still stop the Start handshake")
	require.Error(t, stream.Err())
}

func TestKitTrustPreservesHookEnvironmentBoundary(t *testing.T) {
	t.Parallel()
	for _, workload := range []bool{false, true} {
		for _, hasTrust := range []bool{false, true} {
			original := guest.Request{Env: map[string]string{"DECLARED": "value"}}
			got := kitRequestWithTrust(original, workload, hasTrust)
			require.Equal(t, "value", got.Env["DECLARED"])
			if workload && hasTrust {
				require.Equal(t, "/etc/ssl/certs/ca-certificates.crt", got.Env["NODE_EXTRA_CA_CERTS"])
			} else {
				require.NotContains(t, got.Env, "NODE_EXTRA_CA_CERTS")
			}
			require.Equal(t, map[string]string{"DECLARED": "value"}, original.Env, "trust configuration must not mutate the compiled plan")
		}
	}
	declared := guest.Request{Env: map[string]string{"NODE_EXTRA_CA_CERTS": "/declared/ca.pem"}}
	require.Equal(t, declared, kitRequestWithTrust(declared, false, true), "hooks retain explicitly declared values")
}
