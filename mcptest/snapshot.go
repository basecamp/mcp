package mcptest

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite testdata snapshots from current output")

// Snapshot compares got against the golden file at path, so a surface change
// shows its full effect as a reviewable diff. Run the test with -update to
// rewrite the file from current output.
func Snapshot(t testing.TB, path string, got []byte) {
	t.Helper()

	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}

	expected, err := os.ReadFile(path)
	require.NoError(t, err, "missing snapshot %s; regenerate with -update", path)
	assert.Equal(t, string(expected), string(got),
		"output drifted from snapshot %s; if intended, regenerate with -update", path)
}
