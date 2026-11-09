package metadata

import (
	"strings"
	"testing"

	e2b "github.com/docker/kits-e2b"
	"github.com/stretchr/testify/require"
)

func TestKitMetadataSurvivesSnapshotCopies(t *testing.T) {
	t.Parallel()
	original := Template{Version: CurrentVersion, Kit: &e2b.Bundle{Passwd: "account database"}, KitInstalled: true, KitInstanceID: "instance-a"}
	for _, copied := range []Template{
		original.NewVersionTemplate(TemplateMetadata{BuildID: "new"}),
		original.SameVersionTemplate(TemplateMetadata{BuildID: "pause"}),
		original.WithVcpuCount(4),
		original.WithPrefetch(&Prefetch{}),
		original.WithPrefetch(nil),
	} {
		reader, err := serialize(copied)
		require.NoError(t, err)
		actual, err := deserialize(reader)
		require.NoError(t, err)
		require.Equal(t, original.Kit, actual.Kit)
		require.True(t, actual.KitInstalled)
		require.Equal(t, "instance-a", actual.KitInstanceID)
	}
}

func TestDeprecatedMetadataCannotDiscardKit(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`{"version":1,"kit":{}}`, `{"version":"old","KIT":{}}`, `{"kit_installed":true}`} {
		_, err := deserialize(strings.NewReader(raw))
		require.Error(t, err)
	}
}
