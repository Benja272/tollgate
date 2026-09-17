package ports

import (
	"context"
	"errors"
)

// Checkout prepares a workspace as a pinned, detached git worktree at
// exactly sha (ADR-0006 §2). Implementations must run every git
// invocation with repository hooks disabled — a pinned, otherwise-untrusted
// commit must never run code from its own tree during checkout.
type Checkout interface {
	Checkout(ctx context.Context, repo, sha, path string) error
}

// ReservedPathSuffix marks the overlay's durable-write temp files. It is
// reserved for the whole workspace: a pinned tree holding a path that ends
// in it is refused, so a temp-suffixed file in a workspace is always a
// leftover of an interrupted overlay.
const ReservedPathSuffix = ".tollgate.tmp"

// Sentinels a Checkout implementation returns. All are non-retryable:
// retrying cannot fix an invalid repo, an unresolvable ref, a workspace path
// that conflicts with the requested commit, or a tree that uses a reserved
// name. A done context or a missing git binary is never reported as one of
// them.
var (
	// ErrInvalidRepo means repo is not a valid local git repository.
	ErrInvalidRepo = errors.New("ports: repo is not a valid git repository")
	// ErrRefNotFound means sha does not resolve to a commit in repo.
	ErrRefNotFound = errors.New("ports: source ref not found in repo")
	// ErrCheckoutConflict means path already exists but is not a worktree
	// cleanly reusable for sha (wrong commit, dirty tree, or not a worktree
	// at all).
	ErrCheckoutConflict = errors.New("ports: workspace path conflicts with the requested checkout")

	// ErrReservedPath means the pinned tree holds a path with a segment
	// ending in ReservedPathSuffix.
	ErrReservedPath = errors.New("ports: pinned tree holds a path reserved for overlay temp files")
)
