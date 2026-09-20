// Package gitcli adapts real git (shelled out) to the ports.Checkout
// interface: a pinned, detached worktree checkout with repository hooks
// disabled (ADR-0006 §2).
package gitcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Benja272/tollgate/internal/ports"
)

// Checkout implements ports.Checkout with real git. Every invocation
// carries `-c core.hooksPath=/dev/null`: `git worktree add` fires
// post-checkout, and a pinned, otherwise-untrusted commit must never run
// code from its own tree.
type Checkout struct{}

var _ ports.Checkout = Checkout{}

func (Checkout) Checkout(ctx context.Context, repo, sha, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%w: workspace path %q must be absolute", ports.ErrCheckoutConflict, path)
	}
	if err := probeRepo(ctx, repo); err != nil {
		return err
	}
	if err := probeCommit(ctx, repo, sha); err != nil {
		return err
	}
	if err := refuseReservedPaths(ctx, repo, sha); err != nil {
		return err
	}

	// Lstat, not Stat: a workspace path that is itself a symlink could point
	// at another job's worktree, and the overlay would write there.
	switch info, statErr := os.Lstat(path); {
	case os.IsNotExist(statErr):
		return freshWorktree(ctx, repo, sha, path)
	case statErr != nil:
		return fmt.Errorf("checkout: stat workspace: %w", statErr)
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%w: %s is a symlink", ports.ErrCheckoutConflict, path)
	case !info.IsDir():
		return fmt.Errorf("%w: %s exists and is not a directory", ports.ErrCheckoutConflict, path)
	default:
		return reuseOrConflict(ctx, repo, sha, path)
	}
}

// git runs one git invocation in dir with repository hooks disabled,
// returning trimmed combined output.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// permanent wraps a failed git call in sentinel, a non-retryable port
// error, unless the failure is transient: a done context (the worker is
// shutting down, or the attempt timed out) or a missing git binary. Those
// stay plain, retryable errors.
func permanent(ctx context.Context, sentinel, err error, format string, args ...any) error {
	what := fmt.Sprintf(format, args...)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("checkout: %s: %w (%w)", what, ctxErr, err)
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("checkout: %s: %w", what, err)
	}
	return fmt.Errorf("%w: %s: %v", sentinel, what, err)
}

func probeRepo(ctx context.Context, repo string) error {
	if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return permanent(ctx, ports.ErrInvalidRepo, err, "%s", repo)
	}
	return nil
}

func probeCommit(ctx context.Context, repo, sha string) error {
	if _, err := git(ctx, repo, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return permanent(ctx, ports.ErrRefNotFound, err, "%s@%s", repo, sha)
	}
	return nil
}

// refuseReservedPaths rejects a pinned tree holding any path with a segment
// that ends in the overlay's reserved temp suffix: the overlay treats such
// names as its own leftovers and removes them.
func refuseReservedPaths(ctx context.Context, repo, sha string) error {
	out, err := git(ctx, repo, "ls-tree", "-r", "-z", "--name-only", sha)
	if err != nil {
		return permanent(ctx, ports.ErrRefNotFound, err, "list tree %s@%s", repo, sha)
	}
	for _, rel := range strings.Split(out, "\x00") {
		for _, seg := range strings.Split(rel, "/") {
			if strings.HasSuffix(seg, ports.ReservedPathSuffix) {
				return fmt.Errorf("%w: %s@%s tracks %q", ports.ErrReservedPath, repo, sha, rel)
			}
		}
	}
	return nil
}

func freshWorktree(ctx context.Context, repo, sha, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("checkout: prepare workspace parent: %w", err)
	}
	if out, err := git(ctx, repo, "worktree", "add", "--detach", "--", path, sha); err != nil {
		return fmt.Errorf("checkout: worktree add: %w: %s", err, out)
	}
	return nil
}

// reuseOrConflict implements the reuse rule (ADR-0006 "Strict checkout
// reuse"): an existing path is reused only if it is the root of a linked
// worktree of the SAME repo, its HEAD is exactly sha, and it has no
// changes of any kind. Anything else is ErrCheckoutConflict: a
// subdirectory of a worktree, or the repository's own main worktree, is
// never a checkout tollgate created. Paths are compared after resolving
// symlinks. "No changes" includes ignored and untracked files and edits
// hidden from git status by assume-unchanged or skip-worktree.
func reuseOrConflict(ctx context.Context, repo, sha, path string) error {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: resolve %s: %v", ports.ErrCheckoutConflict, path, err)
	}
	topLevel, err := resolvedGitPath(ctx, path, "--show-toplevel")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s is not a git worktree", path)
	}
	if topLevel != resolvedPath {
		return fmt.Errorf("%w: %s is inside the worktree %s, not its root", ports.ErrCheckoutConflict, path, topLevel)
	}

	repoCommonDir, err := resolvedGitPath(ctx, repo, "--git-common-dir")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "resolve repo common dir")
	}
	wsCommonDir, err := resolvedGitPath(ctx, path, "--git-common-dir")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s is not a git worktree", path)
	}
	if repoCommonDir != wsCommonDir {
		return fmt.Errorf("%w: %s belongs to a different repository", ports.ErrCheckoutConflict, path)
	}
	wsGitDir, err := resolvedGitPath(ctx, path, "--git-dir")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s: resolve git dir", path)
	}
	if wsGitDir == wsCommonDir {
		return fmt.Errorf("%w: %s is the repository's main worktree, not a linked one", ports.ErrCheckoutConflict, path)
	}

	head, err := git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s: resolve HEAD", path)
	}
	if head != sha {
		return fmt.Errorf("%w: %s is checked out at %s, not %s", ports.ErrCheckoutConflict, path, head, sha)
	}

	status, err := git(ctx, path, "status", "--porcelain", "--ignored", "--untracked-files=all")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s: git status", path)
	}
	if status != "" {
		return fmt.Errorf("%w: %s has changes, untracked or ignored files", ports.ErrCheckoutConflict, path)
	}

	// git status trusts these index flags and never looks at the files they
	// cover, so an edit behind one would pass as clean.
	entries, err := git(ctx, path, "ls-files", "-v", "-z")
	if err != nil {
		return permanent(ctx, ports.ErrCheckoutConflict, err, "%s: git ls-files", path)
	}
	for _, entry := range strings.Split(entries, "\x00") {
		if entry == "" {
			continue
		}
		if tag := entry[0]; tag == 'S' || (tag >= 'a' && tag <= 'z') {
			return fmt.Errorf("%w: %s marks %q assume-unchanged or skip-worktree", ports.ErrCheckoutConflict, path, entry[2:])
		}
	}
	return nil
}

// resolvedGitPath runs `git rev-parse <flag>` in dir and returns the path it
// names as an absolute, symlink-resolved path, so two spellings of one
// directory compare equal.
func resolvedGitPath(ctx context.Context, dir, flag string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", flag)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(dir, out)
	}
	return filepath.EvalSymlinks(out)
}
