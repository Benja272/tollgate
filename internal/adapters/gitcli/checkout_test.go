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

// Reuse must only accept the root of a linked worktree of the same repo:
// a subdirectory of a matching worktree, or the repository's own main
// worktree, passes the old common-dir/HEAD/status checks but is not a
// checkout tollgate created.
func TestCheckout_ReuseRequiresLinkedWorktreeRoot(t *testing.T) {
	t.Run("subdirectory of a matching worktree", func(t *testing.T) {
		repo, _, sha2 := setupRepo(t)
		ws := filepath.Join(t.TempDir(), "ws")
		c := Checkout{}
		require.NoError(t, c.Checkout(context.Background(), repo, sha2, ws))
		sub := filepath.Join(ws, "sub")
		require.NoError(t, os.Mkdir(sub, 0o755)) // empty: git status stays clean

		err := c.Checkout(context.Background(), repo, sha2, sub)
		require.ErrorIs(t, err, ports.ErrCheckoutConflict)
	})
	t.Run("the repository's main worktree", func(t *testing.T) {
		repo, _, sha2 := setupRepo(t)

		err := Checkout{}.Checkout(context.Background(), repo, sha2, repo)
		require.ErrorIs(t, err, ports.ErrCheckoutConflict)
	})
}

// A repo or workspace path reached through a symlink names the same
// repository; reuse must compare resolved paths.
func TestCheckout_ReuseThroughSymlinkedPaths(t *testing.T) {
	repo, _, sha2 := setupRepo(t)
	links := t.TempDir()
	repoLink := filepath.Join(links, "repo")
	require.NoError(t, os.Symlink(repo, repoLink))
	realRoot := t.TempDir()
	rootLink := filepath.Join(links, "root")
	require.NoError(t, os.Symlink(realRoot, rootLink))
	ws := filepath.Join(rootLink, "ws")
	c := Checkout{}

	require.NoError(t, c.Checkout(context.Background(), repoLink, sha2, ws))
	require.NoError(t, c.Checkout(context.Background(), repoLink, sha2, ws), "a retry must reuse the worktree")
	require.NoError(t, c.Checkout(context.Background(), repo, sha2, ws), "the resolved repo path is the same repo")
}

// commitIn adds a commit to repo with the given files and returns its SHA.
func commitIn(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	for rel, content := range files {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644))
	}
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, out)
		return strings.TrimSpace(string(out))
	}
	run("add", "-A")
	run("commit", "-q", "-m", "more")
	return run("rev-parse", "HEAD")
}

// A reused worktree must be exactly the pinned tree: git status hides
// ignored files and edits behind assume-unchanged or skip-worktree
// (review R7, reproduced with an edited, hidden file.txt).
func TestCheckout_ReuseRefusesHiddenModifications(t *testing.T) {
	cases := map[string]func(t *testing.T, repo, ws string){
		"ignored file": func(t *testing.T, repo, ws string) {
			require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("*.local\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(ws, "engine.local"), []byte("x"), 0o644))
		},
		"untracked file in a new directory": func(t *testing.T, repo, ws string) {
			require.NoError(t, os.MkdirAll(filepath.Join(ws, "new", "deep"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(ws, "new", "deep", "f"), []byte("x"), 0o644))
		},
		"edit hidden by assume-unchanged": func(t *testing.T, repo, ws string) {
			gitOutput(t, ws, "update-index", "--assume-unchanged", "file.txt")
			require.NoError(t, os.WriteFile(filepath.Join(ws, "file.txt"), []byte("engine edited"), 0o644))
		},
		"edit hidden by skip-worktree": func(t *testing.T, repo, ws string) {
			gitOutput(t, ws, "update-index", "--skip-worktree", "file.txt")
			require.NoError(t, os.WriteFile(filepath.Join(ws, "file.txt"), []byte("engine edited"), 0o644))
		},
		"assume-unchanged flag without an edit": func(t *testing.T, repo, ws string) {
			gitOutput(t, ws, "update-index", "--assume-unchanged", "file.txt")
		},
	}
	for name, hide := range cases {
		t.Run(name, func(t *testing.T) {
			repo, _, sha2 := setupRepo(t)
			ws := filepath.Join(t.TempDir(), "ws")
			c := Checkout{}
			require.NoError(t, c.Checkout(context.Background(), repo, sha2, ws))
			require.Empty(t, gitOutput(t, ws, "status", "--porcelain"), "precondition: plain status looks clean")
			hide(t, repo, ws)

			err := c.Checkout(context.Background(), repo, sha2, ws)
			require.ErrorIs(t, err, ports.ErrCheckoutConflict)
		})
	}
}

// A cancelled context or a missing git binary is not an invalid repo: it
// must stay retryable (review R9, reproduced).
func TestCheckout_TransientFailures_NotPermanentSentinels(t *testing.T) {
	permanent := []error{ports.ErrInvalidRepo, ports.ErrRefNotFound, ports.ErrCheckoutConflict, ports.ErrReservedPath}
	requireTransient := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		for _, sentinel := range permanent {
			require.NotErrorIs(t, err, sentinel)
		}
	}

	t.Run("cancelled context", func(t *testing.T) {
		repo, _, sha2 := setupRepo(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := Checkout{}.Checkout(ctx, repo, sha2, filepath.Join(t.TempDir(), "ws"))
		requireTransient(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})
	t.Run("cancelled context on reuse", func(t *testing.T) {
		repo, _, sha2 := setupRepo(t)
		ws := filepath.Join(t.TempDir(), "ws")
		require.NoError(t, Checkout{}.Checkout(context.Background(), repo, sha2, ws))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		requireTransient(t, Checkout{}.Checkout(ctx, repo, sha2, ws))
	})
	t.Run("git binary missing", func(t *testing.T) {
		repo, _, sha2 := setupRepo(t)
		t.Setenv("PATH", t.TempDir())
		err := Checkout{}.Checkout(context.Background(), repo, sha2, filepath.Join(t.TempDir(), "ws"))
		requireTransient(t, err)
		require.ErrorIs(t, err, exec.ErrNotFound)
	})
}

// The temp-file suffix is reserved for the whole workspace, so a pinned
// tree holding such a path is refused before any worktree exists (review
// R10: the overlay would otherwise delete a tracked .f.tollgate.tmp).
func TestCheckout_TreeWithReservedSuffix_RefusedBeforeCheckout(t *testing.T) {
	for _, rel := range []string{"output/.f" + ports.ReservedPathSuffix, "dir" + ports.ReservedPathSuffix + "/f"} {
		t.Run(rel, func(t *testing.T) {
			repo, _, _ := setupRepo(t)
			sha := commitIn(t, repo, map[string]string{rel: "tracked"})
			ws := filepath.Join(t.TempDir(), "ws")

			err := Checkout{}.Checkout(context.Background(), repo, sha, ws)

			require.ErrorIs(t, err, ports.ErrReservedPath)
			_, statErr := os.Lstat(ws)
			require.True(t, os.IsNotExist(statErr), "no worktree may be created")
		})
	}
}

// A workspace path that is itself a symlink — even to a matching worktree —
// would send the overlay into another job's workspace (review R11).
func TestCheckout_WorkspacePathIsASymlink_Refused(t *testing.T) {
	repo, _, sha2 := setupRepo(t)
	other := filepath.Join(t.TempDir(), "other-job")
	require.NoError(t, Checkout{}.Checkout(context.Background(), repo, sha2, other))
	ws := filepath.Join(t.TempDir(), "ws")
	require.NoError(t, os.Symlink(other, ws))

	err := Checkout{}.Checkout(context.Background(), repo, sha2, ws)

	require.ErrorIs(t, err, ports.ErrCheckoutConflict)
}

// The workspace path follows a "--" in `worktree add`. Paths are always
// absolute today, so the terminator is pinned on the argv itself.
func TestCheckout_WorktreeAddTerminatesOptionsBeforeThePath(t *testing.T) {
	repo, _, sha2 := setupRepo(t)
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	bin := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "args")
	wrapper := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logFile + "\nexec " + realGit + " \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o755))
	t.Setenv("PATH", bin)
	ws := filepath.Join(t.TempDir(), "ws")

	require.NoError(t, Checkout{}.Checkout(context.Background(), repo, sha2, ws))

	raw, err := os.ReadFile(logFile)
	require.NoError(t, err)
	require.Contains(t, string(raw), "worktree add --detach -- "+ws+" "+sha2)
}

// A relative workspace path is refused before any git runs: the path is
// handed to git, where "-x" or a path relative to an unknown working
// directory could even read as an option.
func TestCheckout_RelativeWorkspacePath_Refused(t *testing.T) {
	repo, sha, _ := setupRepo(t)

	err := Checkout{}.Checkout(context.Background(), repo, sha, filepath.Join("relative", "workspace"))

	require.ErrorIs(t, err, ports.ErrCheckoutConflict)
	require.NoDirExists(t, filepath.Join("relative", "workspace"))
}
