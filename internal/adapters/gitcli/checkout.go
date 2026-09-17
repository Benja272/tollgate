// Package gitcli adapts real git (shelled out) to the ports.Checkout
// interface: a pinned, detached worktree checkout with repository hooks
// disabled (ADR-0006 §2).
package gitcli

import (
	"bytes"
	"context"
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
	if err := probeRepo(ctx, repo); err != nil {
		return err
	}
	if err := probeCommit(ctx, repo, sha); err != nil {
		return err
	}

	switch info, statErr := os.Stat(path); {
	case os.IsNotExist(statErr):
		return freshWorktree(ctx, repo, sha, path)
	case statErr != nil:
		return fmt.Errorf("checkout: stat workspace: %w", statErr)
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

func probeRepo(ctx context.Context, repo string) error {
	if _, err := git(ctx, repo, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("%w: %s: %v", ports.ErrInvalidRepo, repo, err)
	}
	return nil
}

func probeCommit(ctx context.Context, repo, sha string) error {
	if _, err := git(ctx, repo, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("%w: %s@%s: %v", ports.ErrRefNotFound, repo, sha, err)
	}
	return nil
}

func freshWorktree(ctx context.Context, repo, sha, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("checkout: prepare workspace parent: %w", err)
	}
	if _, err := git(ctx, repo, "worktree", "add", "--detach", "--", path, sha); err != nil {
		return fmt.Errorf("checkout: worktree add: %w", err)
	}
	return nil
}

// reuseOrConflict implements the reuse rule (ADR-0006 "Strict checkout
// reuse"): an existing path is reused only if it is the root of a linked
// worktree of the SAME repo, its HEAD is exactly sha, and it has no
// uncommitted changes. Anything else is ErrCheckoutConflict: a subdirectory
// of a worktree, or the repository's own main worktree, is never a
// checkout tollgate created. Paths are compared after resolving symlinks.
func reuseOrConflict(ctx context.Context, repo, sha, path string) error {
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("%w: resolve %s: %v", ports.ErrCheckoutConflict, path, err)
	}
	topLevel, err := resolvedGitPath(ctx, path, "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: %s is not a git worktree: %v", ports.ErrCheckoutConflict, path, err)
	}
	if topLevel != resolvedPath {
		return fmt.Errorf("%w: %s is inside the worktree %s, not its root", ports.ErrCheckoutConflict, path, topLevel)
	}

	repoCommonDir, err := resolvedGitPath(ctx, repo, "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%w: resolve repo common dir: %v", ports.ErrCheckoutConflict, err)
	}
	wsCommonDir, err := resolvedGitPath(ctx, path, "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%w: %s is not a git worktree: %v", ports.ErrCheckoutConflict, path, err)
	}
	if repoCommonDir != wsCommonDir {
		return fmt.Errorf("%w: %s belongs to a different repository", ports.ErrCheckoutConflict, path)
	}
	wsGitDir, err := resolvedGitPath(ctx, path, "--git-dir")
	if err != nil {
		return fmt.Errorf("%w: %s: resolve git dir: %v", ports.ErrCheckoutConflict, path, err)
	}
	if wsGitDir == wsCommonDir {
		return fmt.Errorf("%w: %s is the repository's main worktree, not a linked one", ports.ErrCheckoutConflict, path)
	}

	head, err := git(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %s: resolve HEAD: %v", ports.ErrCheckoutConflict, path, err)
	}
	if head != sha {
		return fmt.Errorf("%w: %s is checked out at %s, not %s", ports.ErrCheckoutConflict, path, head, sha)
	}

	status, err := git(ctx, path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("%w: %s: git status: %v", ports.ErrCheckoutConflict, path, err)
	}
	if status != "" {
		return fmt.Errorf("%w: %s has uncommitted changes", ports.ErrCheckoutConflict, path)
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
