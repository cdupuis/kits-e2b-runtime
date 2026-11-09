package kit

import (
	"testing"

	e2b "github.com/docker/kits-e2b"
	"github.com/docker/sandbox-kit-spec/v3/fetch"
	"github.com/docker/sandbox-kit-spec/v3/resolve"
	"github.com/docker/sandbox-kit-spec/v3/spec"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
)

func TestCredentialPlanCannotRunWithoutInjectionBackend(t *testing.T) {
	t.Parallel()
	imageDigest := digest.FromString("fixture")
	bundle := &e2b.Bundle{
		Plan: &e2b.Plan{
			Version: e2b.PlanVersion, Stage: "image-import-only", Platform: ocispec.Platform{OS: "linux", Architecture: "arm64"}, Image: ocispec.Descriptor{Digest: imageDigest}, CandidateLock: &resolve.Lock{},
			WorkingDir: "/root/workspace", ImageConfig: ocispec.ImageConfig{User: "0", Cmd: []string{"/bin/sh"}}, Environment: map[string]string{"PATH": "/usr/bin:/bin", "HOME": "/root"},
			Resolved: &fetch.Resolved{Descriptor: &spec.Descriptor{Capabilities: []spec.Capability{{Type: spec.CapabilitySbx}}}},
		},
		Receipt: e2b.ImportReceipt{TemplateID: "native", BuildID: "native", Image: "example.com/kit@" + imageDigest.String()},
		Passwd:  "root:x:0:0::/root:/bin/sh\n", Group: "root:x:0:\n",
	}
	_, err := Admit(bundle, false)
	require.NoError(t, err)
	bundle.Plan.Options.SecretBindings = map[string]string{"test": "external-key"}
	bundle.Plan.Resolved.Descriptor.Capabilities = append(bundle.Plan.Resolved.Descriptor.Capabilities,
		spec.Capability{Type: spec.CapabilityNetworkPolicy, Config: map[string]any{"runtime": map[string]any{"allow": []string{"api.example.com"}}}},
		spec.Capability{Type: spec.CapabilityCredential, Config: map[string]any{"service": "test", "phase": "runtime", "apiKey": map[string]any{"name": "TEST_TOKEN", "inject": []any{map[string]any{"domain": "api.example.com", "header": "x-api-key", "format": "%s"}}}}},
	)
	plan, err := bundle.ExecutionPlan()
	require.NoError(t, err)
	require.Equal(t, "proxy-managed", plan.Workload.Env["TEST_TOKEN"])
	require.NotEmpty(t, plan.Network.Runtime.Rules)
	_, err = Admit(bundle, false)
	require.ErrorContains(t, err, "secret-injection backend")
	_, err = Admit(bundle, true)
	require.NoError(t, err)
}
