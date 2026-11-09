package kitcredentials

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFileStoreIsolationRotationAndRemoval(t *testing.T) {
	t.Parallel()
	project, other := uuid.NewString(), uuid.NewString()
	path := filepath.Join(t.TempDir(), "secrets.json")
	write := func(value string) {
		t.Helper()
		data, err := json.Marshal(document{Projects: map[string]map[string]string{project: {"api-key": value}, other: {"api-key": "other-project-value"}}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path+".new", data, 0o600))
		require.NoError(t, os.Rename(path+".new", path))
	}
	write("first-value")
	s, err := NewFileStore(path)
	require.NoError(t, err)
	value, err := Expand(t.Context(), s, project, "Bearer ${e2b.secrets.API-Key}")
	require.NoError(t, err)
	require.Equal(t, "Bearer first-value", value)
	value, err = s.Resolve(t.Context(), other, "api-key")
	require.NoError(t, err)
	require.Equal(t, "other-project-value", value)
	_, err = s.Resolve(t.Context(), uuid.NewString(), "api-key")
	require.Error(t, err)
	write("rotated-value")
	value, err = s.Resolve(t.Context(), project, "api-key")
	require.NoError(t, err)
	require.Equal(t, "rotated-value", value)
	require.NoError(t, os.Remove(path))
	_, err = s.Resolve(t.Context(), project, "api-key")
	require.ErrorIs(t, err, errUnavailable)
}

func TestFileStoreRejectsUnsafeFilesAndContents(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, contents string
		mode           os.FileMode
	}{
		{"public file", `{"projects":{}}`, 0o644},
		{"missing projects", `{}`, 0o600},
		{"invalid project", `{"projects":{"caller":{"key":"value"}}}`, 0o600},
		{"invalid name", `{"projects":{"11111111-1111-4111-8111-111111111111":{"../key":"value"}}}`, 0o600},
		{"newline", `{"projects":{"11111111-1111-4111-8111-111111111111":{"key":"hidden\r\nvalue"}}}`, 0o600},
		{"trailing data", `{"projects":{}} {}`, 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store")
			require.NoError(t, os.WriteFile(path, []byte(tc.contents), tc.mode))
			_, err := NewFileStore(path)
			require.ErrorIs(t, err, errUnavailable)
		})
	}
	path := filepath.Join(t.TempDir(), "store")
	require.NoError(t, os.WriteFile(path, []byte(`{"projects":{}}`), 0o600))
	require.NoError(t, os.Symlink(path, path+".link"))
	_, err := NewFileStore(path + ".link")
	require.ErrorIs(t, err, errUnavailable)
}

func TestExpandRejectsUnboundInterpolation(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"literal", "${other.key}", "${e2b.secrets.key", "${e2b.secrets.key}${e2b.secrets.other}", "${e2b.secrets.../key}"} {
		_, err := Expand(t.Context(), nil, "project", format)
		require.Error(t, err)
	}
}
