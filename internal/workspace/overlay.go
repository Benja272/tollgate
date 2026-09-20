// Package workspace applies overlays — files prepared outside a job's
// checkout — onto the checked-out workspace, bounded to declared
// destination roots and durable against a mid-write crash (ADR-0006 §3, §4).
//
// Writes go through directory file descriptors opened one component at a
// time with O_NOFOLLOW, so a symlink anywhere below the workspace — tracked
// at the pinned commit or planted after the pre-check — can never redirect a
// write. Writing therefore needs the Unix *at(2) syscall family; elsewhere
// Apply fails with errors.ErrUnsupported.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/Benja272/tollgate/internal/ports"
)

// Overlay places the file or directory tree at Source (absolute, outside
// the workspace) at Dest (relative to the workspace, inside a declared
// destination root).
type Overlay struct {
	Source string
	Dest   string
}

// Sentinels Apply and ValidatePaths return. Both are non-retryable: retrying
// cannot fix a path that escapes every declared root, or a source that is
// missing or is not a plain file/directory tree.
var (
	// ErrOutsideRoots means a destination — or an existing path component of
	// one, including a symlink found while walking down to it — is not
	// inside any declared root, or can never be written there.
	ErrOutsideRoots = errors.New("workspace: destination is outside every declared root")
	// ErrUnsupportedSource means a source is missing, or is or contains
	// something that is not a regular file or directory (a symlink, device,
	// socket, or fifo), or contains a name the overlay reserves: a ".git"
	// segment in any letter case, or the temp-file suffix.
	ErrUnsupportedSource = errors.New("workspace: source is missing or not a plain file/directory")
	// ErrDestinationConflict means the workspace, or the overlay set
	// itself, holds something where an overlay needs a different kind of
	// entry: a directory where a file goes, a file on a directory's path, a
	// file and a directory at one path, or a name too long for its temp
	// name. Retrying cannot fix it.
	ErrDestinationConflict = errors.New("workspace: destination conflicts with an existing or planned entry")
)

// maxNameBytes is the longest file name the overlay writes (NAME_MAX on the
// filesystems tollgate targets); the temp name is the longest one it needs.
const maxNameBytes = 255

// tmpSuffix marks a durable-write temp file so a crash mid-copy is
// recognizable. It is reserved for the whole workspace: no source entry or
// destination may end in it, and the checkout refuses a pinned tree that
// holds such a path, so any temp-suffixed name found in a workspace is a
// leftover of an earlier attempt and safe to remove.
const tmpSuffix = ports.ReservedPathSuffix

// Test seams. afterPrecheckHook runs between the pre-check and the first
// write, so tests can plant what a concurrent writer could. The other hooks
// observe fsyncs and renames by workspace-relative path.
var (
	afterPrecheckHook func()
	syncedDirHook     func(rel string)
	syncedFileHook    func(rel string)
	renamedFileHook   func(rel string)
)

// ValidatePaths is the pure path-validation half of ADR-0006 §4: every
// destination root and overlay destination must be a clean, local
// (non-escaping, non-absolute) relative path, must not be "." and must not
// contain a ".git" segment (in any letter case) or a segment ending in the
// temp suffix; every overlay destination must fall inside a declared root
// (matched by exact equality or by a "/"-bounded prefix, so a root "jobs"
// never matches a destination "jobsX/..."); every overlay source must be
// absolute. It never touches the filesystem, so workflow code may call it.
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
// relative, non-"." path free of reserved segments.
func validateRelPath(p string) (string, error) {
	clean := filepath.Clean(p)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("%w: %q escapes the workspace", ErrOutsideRoots, p)
	}
	if clean == "." {
		return "", fmt.Errorf("%w: %q resolves to the workspace root itself", ErrOutsideRoots, p)
	}
	if seg, bad := reservedSegment(clean); bad {
		return "", fmt.Errorf("%w: %q contains the reserved path segment %q", ErrOutsideRoots, p, seg)
	}
	return clean, nil
}

// reservedSegment reports the first segment of rel that the overlay never
// writes: ".git" in any letter case (case-insensitive filesystems resolve
// ".GIT" to the repository's own metadata), or a temp-file name.
func reservedSegment(rel string) (string, bool) {
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if strings.EqualFold(seg, ".git") || strings.HasSuffix(seg, tmpSuffix) {
			return seg, true
		}
	}
	return "", false
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

// plannedFile is one regular file an overlay will write. A file planned
// from a source TREE also carries that tree's declared root and its path
// below it, so the write can open it one component at a time and never
// follow a directory swapped inside the tree after the pre-check. A
// single-file overlay has no root: its whole path is what the caller
// declared.
type plannedFile struct {
	src  string
	root string
	rel  string
	dest string
	perm fs.FileMode
}

// plan is everything one Apply call will create or write, computed and
// checked before the first write.
type plan struct {
	dirs  []string // destination directories, parents before children
	files []plannedFile
}

// Apply places every overlay inside workspace, bounded to roots. It never
// writes a single byte until every overlay has passed the path validation
// and the pre-check: every source tree is walked (no symlink, no
// non-regular file, no reserved name), and every future destination path
// component — including each file's temp name — is Lstat'ed; an existing
// symlink on any of them aborts the whole call.
//
// The pre-check is not the only defense. Every write opens its directories
// one component at a time with O_NOFOLLOW, removes any existing temp path,
// creates the temp file with O_EXCL, and changes its mode through the open
// handle — never by name — so a symlink planted after the pre-check is
// refused or replaced, never followed.
//
// Each file is written durably (ADR-0006 §3): to a "<dir>/.<base>.tollgate.tmp"
// sibling created writable, filled, given the source's mode, fsynced, then
// renamed over the final name. After all files are placed, every directory
// that received a rename, and the parent of every directory on the path to
// any destination — created by this call or by an earlier, failed one — are
// fsynced. A retry after a partial failure is safe: it
// overwrites the same files by the same sequence, and a leftover temp file,
// whatever its mode, is removed first.
//
// beat, when non-nil, is invoked once per written file so a long copy can
// still heartbeat its enclosing activity.
func Apply(ctx context.Context, workspace string, roots []string, ovs []Overlay, beat func()) error {
	if err := ValidatePaths(roots, ovs); err != nil {
		return err
	}
	p, err := precheck(workspace, roots, ovs)
	if err != nil {
		return err
	}
	if afterPrecheckHook != nil {
		afterPrecheckHook()
	}

	w, err := newWriter(workspace)
	if err != nil {
		return err
	}
	defer w.close()

	for _, d := range p.dirs {
		if err := w.mkdirAll(d); err != nil {
			return err
		}
	}
	for _, f := range p.files {
		if beat != nil {
			beat()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.writeFile(f); err != nil {
			return err
		}
	}
	return w.syncDirs()
}

// precheck walks every source and every future destination, returning the
// full write plan or the first reason the call must not write anything.
func precheck(workspace string, roots []string, ovs []Overlay) (plan, error) {
	var p plan
	for _, r := range roots {
		p.dirs = append(p.dirs, filepath.Clean(r))
	}

	for _, ov := range ovs {
		dest := filepath.Clean(ov.Dest)
		info, err := os.Lstat(ov.Source)
		if err != nil {
			return plan{}, fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, ov.Source, err)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return plan{}, fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, ov.Source)
		case info.Mode().IsRegular():
			for _, r := range roots {
				if dest == filepath.Clean(r) {
					return plan{}, fmt.Errorf("%w: file overlay %q would replace the root directory itself", ErrOutsideRoots, ov.Dest)
				}
			}
			p.files = append(p.files, plannedFile{src: ov.Source, dest: dest, perm: info.Mode().Perm()})
		case info.IsDir():
			if err := planTree(ov.Source, dest, &p); err != nil {
				return plan{}, err
			}
		default:
			return plan{}, fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, ov.Source)
		}
	}

	if err := checkPlanConsistent(p); err != nil {
		return plan{}, err
	}
	for _, d := range p.dirs {
		if err := checkExisting(workspace, d, kindDir); err != nil {
			return plan{}, err
		}
	}
	for _, f := range p.files {
		if err := checkExisting(workspace, f.dest, kindFile); err != nil {
			return plan{}, err
		}
		if err := checkExisting(workspace, tempRel(f.dest), kindTemp); err != nil {
			return plan{}, err
		}
	}
	return p, nil
}

// checkPlanConsistent rejects an overlay set that asks for a file and a
// directory at one path, or for a path below a file, and a file whose temp
// name would be too long.
func checkPlanConsistent(p plan) error {
	files := make(map[string]bool, len(p.files))
	for _, f := range p.files {
		files[f.dest] = true
		if len("."+filepath.Base(f.dest)+tmpSuffix) > maxNameBytes {
			return fmt.Errorf("%w: %s is too long for its temporary name", ErrDestinationConflict, f.dest)
		}
	}
	belowAFile := func(rel string) error {
		for dir := filepath.Dir(rel); dir != "."; dir = filepath.Dir(dir) {
			if files[dir] {
				return fmt.Errorf("%w: %s lies below the file overlay %s", ErrDestinationConflict, rel, dir)
			}
		}
		return nil
	}
	for _, d := range p.dirs {
		if files[d] {
			return fmt.Errorf("%w: %s is both a file and a directory overlay", ErrDestinationConflict, d)
		}
		if err := belowAFile(d); err != nil {
			return err
		}
	}
	for _, f := range p.files {
		if err := belowAFile(f.dest); err != nil {
			return err
		}
	}
	return nil
}

// planTree adds one source directory tree to p, rejecting anything that is
// not a directory or regular file and any reserved name inside it.
func planTree(source, dest string, p *plan) error {
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, path, walkErr)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, path, err)
		}
		if rel != "." {
			if seg, bad := reservedSegment(rel); bad {
				return fmt.Errorf("%w: %s contains the reserved name %q", ErrUnsupportedSource, path, seg)
			}
		}
		target := filepath.Join(dest, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, path)
		case d.IsDir():
			p.dirs = append(p.dirs, target)
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrUnsupportedSource, path, err)
			}
			p.files = append(p.files, plannedFile{
				src: path, root: source, rel: rel, dest: target, perm: info.Mode().Perm(),
			})
		default:
			return fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, path)
		}
		return nil
	})
}

// tempRel is the temp sibling a file destination is written through.
func tempRel(dest string) string {
	return filepath.Join(filepath.Dir(dest), "."+filepath.Base(dest)+tmpSuffix)
}

// entryKind is what an overlay needs at a destination path.
type entryKind int

const (
	kindDir  entryKind = iota // a directory, created if missing
	kindFile                  // a regular file, replaced if present
	kindTemp                  // a temp name: absent, or a leftover regular file
)

// checkExisting Lstats every existing component of workspace/rel, from the
// workspace down. A symlink anywhere is ErrOutsideRoots. An ancestor that
// is not a directory, or a final entry of the wrong kind, is
// ErrDestinationConflict. Once a component is missing, nothing below it
// can exist.
func checkExisting(workspace, rel string, want entryKind) error {
	parts := strings.Split(rel, string(filepath.Separator))
	cur := workspace
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if errors.Is(err, syscall.ENAMETOOLONG) {
			// A name the filesystem cannot even hold is a destination this
			// overlay can never write; retrying changes nothing.
			return fmt.Errorf("%w: %s is too long a name for this filesystem", ErrDestinationConflict, cur)
		}
		if err != nil {
			return fmt.Errorf("workspace: lstat %s: %w", cur, err)
		}
		mode := info.Mode()
		if mode&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrOutsideRoots, cur)
		}
		final := i == len(parts)-1
		switch {
		case !final || want == kindDir:
			if !mode.IsDir() {
				return fmt.Errorf("%w: %s is not a directory", ErrDestinationConflict, cur)
			}
		case !mode.IsRegular():
			return fmt.Errorf("%w: %s exists and is not a regular file", ErrDestinationConflict, cur)
		}
	}
	return nil
}
