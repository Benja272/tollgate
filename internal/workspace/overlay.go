// Package workspace applies overlays — files prepared outside a job's
// checkout — onto the checked-out workspace, bounded to declared
// destination roots and durable against a mid-write crash (ADR-0006 D9-D11).
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Overlay places the file or directory tree at Source (absolute, outside
// the workspace) at Dest (relative to the workspace, inside a declared
// destination root).
type Overlay struct {
	Source string
	Dest   string
}

// Sentinels Apply and ValidatePaths return. Both are non-retryable: retrying
// cannot fix a path that lexically escapes every declared root, or a source
// that is missing or is not a plain file/directory tree.
var (
	// ErrOutsideRoots means a destination — or an existing path component of
	// one, including a symlink found while walking down to it — is not
	// inside any declared root.
	ErrOutsideRoots = errors.New("workspace: destination is outside every declared root")
	// ErrUnsupportedSource means a source is missing, or is or contains
	// something that is not a regular file or directory: a symlink, device,
	// socket, or fifo.
	ErrUnsupportedSource = errors.New("workspace: source is missing or not a plain file/directory")
)

// tmpSuffix marks a durable-write temp file so a crash mid-copy is
// recognizable, and so ValidatePaths/Apply never treat one as real content.
const tmpSuffix = ".tollgate.tmp"

// ValidatePaths is the pure path-validation half of D9: every destination
// root and overlay destination must be a clean, local (non-escaping,
// non-absolute) relative path, must not be "." and must not contain a
// ".git" segment; every overlay destination must fall inside a declared
// root (matched by exact equality or by a "/"-bounded prefix, so a root
// "jobs" never matches a destination "jobsX/..."); every overlay source
// must be absolute.
func ValidatePaths(roots []string, ovs []Overlay) error {
	if len(roots) == 0 {
		return fmt.Errorf("%w: at least one destination root is required", ErrOutsideRoots)
	}

	cleanRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		clean, err := validateRelPath(r)
		if err != nil {
			return err
		}
		cleanRoots = append(cleanRoots, clean)
	}

	for _, ov := range ovs {
		if !filepath.IsAbs(ov.Source) {
			return fmt.Errorf("%w: source %q must be absolute", ErrUnsupportedSource, ov.Source)
		}
		destClean, err := validateRelPath(ov.Dest)
		if err != nil {
			return err
		}
		if !destBelongsToRoot(destClean, cleanRoots) {
			return fmt.Errorf("%w: destination %q is outside every declared root", ErrOutsideRoots, ov.Dest)
		}
	}
	return nil
}

// validateRelPath cleans p and rejects anything that is not a safe,
// relative, non-"." path with no ".git" segment.
func validateRelPath(p string) (string, error) {
	clean := filepath.Clean(p)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("%w: %q escapes the workspace", ErrOutsideRoots, p)
	}
	if clean == "." {
		return "", fmt.Errorf("%w: %q resolves to the workspace root itself", ErrOutsideRoots, p)
	}
	for _, seg := range strings.Split(clean, string(filepath.Separator)) {
		if seg == ".git" {
			return "", fmt.Errorf("%w: %q contains a .git path segment", ErrOutsideRoots, p)
		}
	}
	return clean, nil
}

// destBelongsToRoot reports whether dest is exactly one root or sits below
// one, bounded by a path separator — a plain string prefix would wrongly
// let "jobsX" match a root of "jobs".
func destBelongsToRoot(dest string, roots []string) bool {
	for _, r := range roots {
		if dest == r || strings.HasPrefix(dest, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Apply places every overlay inside workspace, bounded to roots. It never
// writes a single byte until every overlay has passed BOTH the path
// validation (D9) and the symlink pre-check (D10): a destination with any
// existing symlink path component, or a source that is or contains any
// symlink, device, socket, or fifo anywhere in its tree, aborts the whole
// call before anything is written.
//
// Each file is written durably (D11): to a "<dir>/.<base>.tollgate.tmp"
// sibling, fsynced, then renamed over the final name; every directory
// touched by a rename is fsynced once, after all files are placed. A retry
// after a partial failure is safe: existing files are simply overwritten by
// the same durable sequence, and no temp file is ever left behind on either
// success or a later successful retry.
//
// beat, when non-nil, is invoked once per overlay so a long copy can still
// heartbeat its enclosing activity.
func Apply(ctx context.Context, workspace string, roots []string, ovs []Overlay, beat func()) error {
	if err := ValidatePaths(roots, ovs); err != nil {
		return err
	}

	for _, ov := range ovs {
		if err := checkDestNoSymlink(workspace, ov.Dest); err != nil {
			return err
		}
		if err := checkSourceNoSymlink(ov.Source); err != nil {
			return err
		}
	}

	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("workspace: open workspace root: %w", err)
	}
	defer func() { _ = root.Close() }()

	for _, r := range roots {
		if err := root.MkdirAll(r, 0o755); err != nil {
			return fmt.Errorf("workspace: create root %q: %w", r, err)
		}
	}

	touchedDirs := map[string]bool{}
	for _, ov := range ovs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if beat != nil {
			beat()
		}
		if err := copyOverlay(root, ov, touchedDirs); err != nil {
			return err
		}
	}

	for dir := range touchedDirs {
		if err := fsyncDir(root, dir); err != nil {
			return err
		}
	}
	return nil
}

// checkDestNoSymlink Lstats every existing path component of workspace/dest,
// from the workspace down. A missing component is fine — it will be
// created. Any existing component that is a symlink is rejected: reuse (of
// a checked-out worktree) is the only case an overlay destination should
// ever already exist as something other than a plain file or directory.
func checkDestNoSymlink(workspace, dest string) error {
	full := filepath.Join(workspace, dest)
	rel, err := filepath.Rel(workspace, full)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrOutsideRoots, dest, err)
	}
	cur := workspace
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, statErr := os.Lstat(cur)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("workspace: lstat %s: %w", cur, statErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrOutsideRoots, cur)
		}
	}
	return nil
}

// checkSourceNoSymlink rejects a source that is, or anywhere contains, a
// symlink, device, socket, or fifo — the only things a plain file/directory
// overlay should never carry.
func checkSourceNoSymlink(source string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, source, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, source)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, source)
		}
		return nil
	}
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, path, walkErr)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, path)
		}
		if !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, path)
		}
		return nil
	})
}

// copyOverlay copies one overlay's source (a file or a directory tree) to
// its destination inside root, recording every directory it touches so
// Apply can fsync each exactly once.
func copyOverlay(root *os.Root, ov Overlay, touchedDirs map[string]bool) error {
	info, err := os.Lstat(ov.Source)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, ov.Source, err)
	}
	if !info.IsDir() {
		return copyFileInRoot(root, ov.Source, ov.Dest, info, touchedDirs)
	}
	return filepath.WalkDir(ov.Source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(ov.Source, path)
		if relErr != nil {
			return relErr
		}
		destRel := ov.Dest
		if rel != "." {
			destRel = filepath.Join(ov.Dest, rel)
		}
		if d.IsDir() {
			if err := root.MkdirAll(destRel, 0o755); err != nil {
				return fmt.Errorf("workspace: create %s: %w", destRel, err)
			}
			return nil
		}
		fileInfo, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		return copyFileInRoot(root, path, destRel, fileInfo, touchedDirs)
	})
}

// copyFileInRoot writes one file durably: a temp sibling, fsynced, then
// renamed over destRel, preserving srcInfo's mode bits (D11).
func copyFileInRoot(root *os.Root, srcPath, destRel string, srcInfo fs.FileInfo, touchedDirs map[string]bool) error {
	destDir := filepath.Dir(destRel)
	if destDir != "." {
		if err := root.MkdirAll(destDir, 0o755); err != nil {
			return fmt.Errorf("workspace: create %s: %w", destDir, err)
		}
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, srcPath, err)
	}
	defer func() { _ = src.Close() }()

	tmpRel := filepath.Join(destDir, "."+filepath.Base(destRel)+tmpSuffix)
	tmp, err := root.OpenFile(tmpRel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return fmt.Errorf("workspace: create temp file for %s: %w", destRel, err)
	}

	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpRel)
		return fmt.Errorf("workspace: copy %s: %w", destRel, err)
	}
	// Mode bits are preserved explicitly: OpenFile's perm argument is
	// subject to umask, so it alone cannot guarantee an exact match.
	if err := root.Chmod(tmpRel, srcInfo.Mode().Perm()); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpRel)
		return fmt.Errorf("workspace: chmod temp file for %s: %w", destRel, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = root.Remove(tmpRel)
		return fmt.Errorf("workspace: fsync temp file for %s: %w", destRel, err)
	}
	if err := tmp.Close(); err != nil {
		_ = root.Remove(tmpRel)
		return fmt.Errorf("workspace: close temp file for %s: %w", destRel, err)
	}
	if err := root.Rename(tmpRel, destRel); err != nil {
		return fmt.Errorf("workspace: rename into place %s: %w", destRel, err)
	}
	touchedDirs[destDir] = true
	return nil
}

// fsyncDir fsyncs one directory inside root, making the renames into it
// durable against a crash (D11).
func fsyncDir(root *os.Root, dir string) error {
	f, err := root.Open(dir)
	if err != nil {
		return fmt.Errorf("workspace: open dir %s for fsync: %w", dir, err)
	}
	defer func() { _ = f.Close() }()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("workspace: fsync dir %s: %w", dir, err)
	}
	return nil
}
