package gate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeRubric(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rubric.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

const validRubric = `name: default
axes:
  - name: correctness
    description: The change does what the ticket asks without breaking behavior.
    blocking: true
    min_score: 4
  - name: clarity
    description: The change is readable and idiomatic.
    blocking: false
    min_score: 3
`

func TestLoadRubric_ParsesAxes(t *testing.T) {
	r, err := LoadRubric(writeRubric(t, validRubric))

	require.NoError(t, err)
	require.Equal(t, "default", r.Name)
	require.Len(t, r.Axes, 2)
	require.Equal(t, "correctness", r.Axes[0].Name)
	require.True(t, r.Axes[0].Blocking)
	require.Equal(t, 4, r.Axes[0].MinScore)
	require.False(t, r.Axes[1].Blocking)
}

func TestLoadRubric_VersionIsContentHash(t *testing.T) {
	a1, err := LoadRubric(writeRubric(t, validRubric))
	require.NoError(t, err)
	a2, err := LoadRubric(writeRubric(t, validRubric))
	require.NoError(t, err)
	b, err := LoadRubric(writeRubric(t, validRubric+"  # tweaked\n"))
	require.NoError(t, err)

	require.Regexp(t, `^sha256:[0-9a-f]{64}$`, a1.Version)
	require.Equal(t, a1.Version, a2.Version, "same content must yield the same version, regardless of path")
	require.NotEqual(t, a1.Version, b.Version, "any content change must change the version")
}

func TestLoadRubric_RejectsInvalid(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"no axes", "name: empty\naxes: []\n"},
		{"duplicate axis names", "name: dup\naxes:\n  - name: a\n    min_score: 3\n  - name: a\n    min_score: 3\n"},
		{"empty axis name", "name: anon\naxes:\n  - name: \"\"\n    min_score: 3\n"},
		{"min_score below scale", "name: low\naxes:\n  - name: a\n    min_score: 0\n"},
		{"min_score above scale", "name: high\naxes:\n  - name: a\n    min_score: 6\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRubric(writeRubric(t, tc.content))
			require.Error(t, err)
		})
	}
}
