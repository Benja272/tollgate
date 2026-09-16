package gitcli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Benja272/tollgate/internal/ports"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
}

// gitOutput runs a git command in dir and requires it to succeed.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// setupRepo creates a two-commit git repository and returns its path plus
// both commit SHAs.
func setupRepo(t *testing.T) (repo, sha1, sha2 string) {
	t.Helper()
	requireGit(t)
	repo = t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=tollgate-test", "GIT_AUTHOR_EMAIL=test@tollgate.local",
			"GIT_COMMITTER_NAME=tollgate-test", "GIT_COMMITTER_EMAIL=test@tollgate.local",
		)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("v1"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "first")
	sha1 = run("rev-parse", "HEAD")

	require.NoError(t, os.WriteFile(filepath.Join(repo, "file.txt"), []byte("v2"), 0o644))
	run("add", ".")
	run("commit", "-q", "-m", "second")
	sha2 = run("rev-parse", "HEAD")
	return repo, sha1, sha2
}

func TestCheckout_FreshDetachedWorktree(t *testing.T) {
	repo, sha1, _ := setupRepo(t)
	ws := filepath.Join(t.TempDir(), "ws")
	c := Checkout{}

	err := c.Checkout(context.Background(), repo, sha1, ws)
	require.NoError(t, err)

	require.Equal(t, sha1, gitOutput(t, ws, "rev-parse", "HEAD"))
	require.Equal(t, "HEAD", gitOutput(t, ws, "rev-parse", "--abbrev-ref", "HEAD"),
		"checkout must be detached, not on a branch")
}

func TestCheckout_RetryReusesMatchingWorktree_NoReClone(t *testing.T) {
	repo, sha1, _ := setupRepo(t)
	ws := filepath.Join(t.TempDir(), "ws")
	c := Checkout{}

	require.NoError(t, c.Checkout(context.Background(), repo, sha1, ws))
	before := gitOutput(t, repo, "worktree", "list", "--porcelain")

	require.NoError(t, c.Checkout(context.Background(), repo, sha1, ws))
	after := gitOutput(t, repo, "worktree", "list", "--porcelain")

	require.Equal(t, before, after, "a reused worktree must not be re-added (no re-clone)")
	require.Equal(t, sha1, gitOutput(t, ws, "rev-parse", "HEAD"))
}

func TestCheckout_ConflictingPath_NonRetryable(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, repo, sha1, sha2, ws string, c Checkout)
	}{
		{
			name: "different sha checked out",
			setup: func(t *testing.T, repo, sha1, sha2, ws string, c Checkout) {
				require.NoError(t, c.Checkout(context.Background(), repo, sha1, ws))
			},
		},
		{
			name: "dirty tree",
			setup: func(t *testing.T, repo, sha1, sha2, ws string, c Checkout) {
				require.NoError(t, c.Checkout(context.Background(), repo, sha2, ws))
				require.NoError(t, os.WriteFile(filepath.Join(ws, "file.txt"), []byte("dirty"), 0o644))
			},
		},
		{
			name: "non-git directory",
			setup: func(t *testing.T, repo, sha1, sha2, ws string, c Checkout) {
				require.NoError(t, os.MkdirAll(ws, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(ws, "stray.txt"), []byte("x"), 0o644))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, sha1, sha2 := setupRepo(t)
			ws := filepath.Join(t.TempDir(), "ws")
			c := Checkout{}
			tc.setup(t, repo, sha1, sha2, ws, c)

			err := c.Checkout(context.Background(), repo, sha2, ws)
			require.Error(t, err)
			require.ErrorIs(t, err, ports.ErrCheckoutConflict)
		})
	}
}

func TestCheckout_UnknownSHA_Rejected(t *testing.T) {
	repo, _, _ := setupRepo(t)
	ws := filepath.Join(t.TempDir(), "ws")
	c := Checkout{}

	unknown := strings.Repeat("f", 40)
	err := c.Checkout(context.Background(), repo, unknown, ws)

	require.Error(t, err)
	require.ErrorIs(t, err, ports.ErrRefNotFound)
}

func TestCheckout_PathNotARepo_Rejected(t *testing.T) {
	requireGit(t)
	notARepo := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(notARepo, "file.txt"), []byte("x"), 0o644))
	ws := filepath.Join(t.TempDir(), "ws")
	c := Checkout{}

	err := c.Checkout(context.Background(), notARepo, strings.Repeat("a", 40), ws)

	require.Error(t, err)
	require.ErrorIs(t, err, ports.ErrInvalidRepo)
}

func TestCheckout_RunsNoRepositoryHooks(t *testing.T) {
	repo, sha1, _ := setupRepo(t)

	marker := filepath.Join(t.TempDir(), "hook-ran")
	hook := "#!/bin/sh\ntouch \"" + marker + "\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "hooks", "post-checkout"), []byte(hook), 0o755))

	ws := filepath.Join(t.TempDir(), "ws")
	c := Checkout{}
	require.NoError(t, c.Checkout(context.Background(), repo, sha1, ws))

	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "post-checkout hook must never run (core.hooksPath=/dev/null on every invocation)")
}
