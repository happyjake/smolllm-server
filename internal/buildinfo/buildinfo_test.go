package buildinfo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCurrent(t *testing.T) {
	oldServer, oldLibrary, oldClean := serverRevision, libraryRevision, sourceClean
	t.Cleanup(func() { serverRevision, libraryRevision, sourceClean = oldServer, oldLibrary, oldClean })
	serverRevision, libraryRevision, sourceClean = "unknown", "unknown", "false"
	info := Current()
	require.False(t, info.SourceClean)
	require.Equal(t, 1, info.SchemaVersion)
	require.Positive(t, info.PID)
	require.Len(t, info.InstanceID, 32)
	require.Equal(t, info.InstanceID, Current().InstanceID)
	serverRevision, libraryRevision, sourceClean = strings.Repeat("a", 40), strings.Repeat("b", 40), "true"
	require.True(t, Current().SourceClean)
	libraryRevision = "short"
	require.False(t, Current().SourceClean, "incomplete library provenance cannot pass")
}
