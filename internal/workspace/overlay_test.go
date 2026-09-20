package workspace

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePaths_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		roots   []string
		overlay Overlay
		wantErr bool
	}{
		{
			name:    "dest escapes with dotdot",
			roots:   []string{"output"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "output/../../etc/passwd"},
			wantErr: true,
		},
		{
			name:    "absolute dest rejected",
			roots:   []string{"output"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "/output/file.txt"},
			wantErr: true,
		},
		{
			name:    "root of dot rejected",
			roots:   []string{"."},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "file.txt"},
			wantErr: true,
		},
		{
			name:    "root with .git segment rejected",
			roots:   []string{"output/.git/hooks"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "output/.git/hooks/x"},
			wantErr: true,
		},
		{
			name:    "dest with .git segment beyond the root rejected",
			roots:   []string{"output"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "output/.git/config"},
			wantErr: true,
		},
		{
			name:    "prefix collision jobsX vs jobs rejected",
			roots:   []string{"jobs"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "jobsX/file.txt"},
			wantErr: true,
		},
		{
			name:    "relative source rejected",
			roots:   []string{"output"},
			overlay: Overlay{Source: "relative/src.txt", Dest: "output/file.txt"},
			wantErr: true,
		},
		{
			name:    "valid overlay inside declared root accepted",
			roots:   []string{"output"},
			overlay: Overlay{Source: "/abs/src.txt", Dest: "output/file.txt"},
			wantErr: false,
		},
		{
			name:    "valid overlay equal to root accepted",
			roots:   []string{"output/plan.md"},
			overlay: Overlay{Source: "/abs/plan.md", Dest: "output/plan.md"},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePaths(tc.roots, []Overlay{tc.overlay})
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidatePaths_NoRoots_Rejected(t *testing.T) {
	err := ValidatePaths(nil, []Overlay{{Source: "/abs/src.txt", Dest: "file.txt"}})
	require.Error(t, err)
}

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), perm))
}

func TestApply_OverwriteOfTrackedFile_KeepsMode(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))
	// Simulate a git-tracked file already present at the pinned commit, with
	// the default non-executable mode git gives regular files.
	writeFile(t, filepath.Join(ws, "output", "run.sh"), "old content", 0o644)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "run.sh")
	writeFile(t, src, "#!/bin/sh\necho hi\n", 0o755)

	err := Apply(t.Context(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/run.sh"}}, nil)
	require.NoError(t, err)

	info, statErr := os.Stat(filepath.Join(ws, "output", "run.sh"))
	require.NoError(t, statErr)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "overlay must carry the source's mode bits onto the destination")

	got, readErr := os.ReadFile(filepath.Join(ws, "output", "run.sh"))
	require.NoError(t, readErr)
	require.Equal(t, "#!/bin/sh\necho hi\n", string(got))
}

func TestApply_DestinationSymlinkEscape_NoFileWritten(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))
	require.NoError(t, os.Symlink(outside, filepath.Join(ws, "output", "escape")))

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "payload.txt")
	writeFile(t, src, "malicious", 0o644)

	err := Apply(t.Context(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/escape/payload.txt"}}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrOutsideRoots)

	_, statErr := os.Stat(filepath.Join(outside, "payload.txt"))
	require.True(t, os.IsNotExist(statErr), "no file must be written when a destination component is a symlink")
}

func TestApply_SourceSymlinkAnywhereInTree_Rejected_NoFileWritten(t *testing.T) {
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))

	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "real.txt"), "fine", 0o644)
	linkTarget := filepath.Join(srcDir, "real.txt")
	require.NoError(t, os.Symlink(linkTarget, filepath.Join(srcDir, "link.txt")))

	err := Apply(t.Context(), ws, []string{"output"}, []Overlay{{Source: srcDir, Dest: "output/copied"}}, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedSource)

	entries, readErr := os.ReadDir(filepath.Join(ws, "output"))
	require.NoError(t, readErr)
	require.Empty(t, entries, "a rejected source must leave no file written, including its non-symlink siblings")
}

func TestApply_RetryAfterPartialFailure_IdenticalTreeNoTempFilesLeft(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission-denial injection does not apply when running as root")
	}
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))

	srcDir := t.TempDir()
	writeFile(t, filepath.Join(srcDir, "a.txt"), "aaa", 0o644)
	bPath := filepath.Join(srcDir, "b.txt")
	writeFile(t, bPath, "bbb", 0o644)
	writeFile(t, filepath.Join(srcDir, "c.txt"), "ccc", 0o644)

	require.NoError(t, os.Chmod(bPath, 0o000))
	firstErr := Apply(t.Context(), ws, []string{"output"}, []Overlay{{Source: srcDir, Dest: "output/copied"}}, nil)
	require.Error(t, firstErr, "b.txt is unreadable: the copy must fail partway through")

	require.NoError(t, os.Chmod(bPath, 0o644))
	retryErr := Apply(t.Context(), ws, []string{"output"}, []Overlay{{Source: srcDir, Dest: "output/copied"}}, nil)
	require.NoError(t, retryErr, "retry after the source becomes readable again must succeed")

	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		want, err := os.ReadFile(filepath.Join(srcDir, name))
		require.NoError(t, err)
		got, err := os.ReadFile(filepath.Join(ws, "output", "copied", name))
		require.NoError(t, err)
		require.Equal(t, string(want), string(got), "file %s must match the source after retry", name)
	}

	matches, globErr := filepath.Glob(filepath.Join(ws, "output", "copied", ".*.tollgate.tmp"))
	require.NoError(t, globErr)
	require.Empty(t, matches, "no temp files must survive a completed (or retried) apply")
}
