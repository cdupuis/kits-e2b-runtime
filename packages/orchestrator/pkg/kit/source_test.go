package kit

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeSourceRefusesLostPolicy(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{}`, `{"references":[]}`, `{"references":[""]}`, `{"references":[" x"]}`, `{"references":["x"],"options":{"hostOnlyNetwrok":true}}`, `{"references":["x"]} {}`, strings.Repeat("x", (1<<20)+1)} {
		_, err := DecodeSource([]byte(raw))
		require.Error(t, err)
	}
	source, err := DecodeSource([]byte(`{"references":["docker.io/example/kit:1"],"options":{"ephemeralVolumes":true,"ignoreAgentSessions":true}}`))
	require.NoError(t, err)
	require.True(t, source.Options.EphemeralVolumes)
	require.True(t, source.Options.IgnoreAgentSessions)
	require.False(t, source.Options.HostOnlyNetwork)
}

// Opt-in registry integration: tests composition without allocating a VM.
func TestPublishedKitSource(t *testing.T) {
	t.Parallel()
	ref := os.Getenv("E2B_TEST_KIT_REF")
	if ref == "" {
		t.Skip("set E2B_TEST_KIT_REF to exercise OCI admission")
	}
	raw, err := json.Marshal(Source{References: []string{ref}})
	require.NoError(t, err)
	prepared, err := Prepare(t.Context(), raw, "arm64", false)
	require.NoError(t, err)
	defer prepared.Close()
	require.NotNil(t, prepared.Image)
	require.True(t, strings.HasPrefix(prepared.Key, "kit-v3:"))
	plan, err := Admit(prepared.Bundle, false)
	require.NoError(t, err)
	require.Equal(t, "terminal", plan.Interactive.Action)
	require.NotEmpty(t, plan.Interactive.Argv)
}
