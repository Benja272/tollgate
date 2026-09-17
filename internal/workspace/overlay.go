// Package workspace applies overlays — files prepared outside a job's
// checkout — onto the checked-out workspace, bounded to declared
// destination roots and durable against a mid-write crash (ADR-0006 §3, §4).
//
// Writes go through directory file descriptors opened one component at a
// time with O_NOFOLLOW, so a symlink anywhere below the workspace — tracked
// at the pinned commit or planted after the pre-check — can never redirect a
// write. The package therefore needs a Unix *at(2) syscall family.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
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
)

// tmpSuffix marks a durable-write temp file so a crash mid-copy is
// recognizable. No source entry or destination may end in it, so a temp
// name can never collide with real content.
const tmpSuffix = ".tollgate.tmp"

// Test seams. afterPrecheckHook runs between the pre-check and the first
// write, so tests can plant what a concurrent writer could. syncedDirHook
// observes every directory fsync, by workspace-relative path.
var (
	afterPrecheckHook func()
	syncedDirHook     func(rel string)
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

// plannedFile is one regular file an overlay will write.
type plannedFile struct {
	src  string
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
// that received a rename and the parent of every directory this call
// created are fsynced. A retry after a partial failure is safe: it
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if beat != nil {
			beat()
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

	for _, d := range p.dirs {
		if err := checkNoSymlink(workspace, d); err != nil {
			return plan{}, err
		}
	}
	for _, f := range p.files {
		if err := checkNoSymlink(workspace, f.dest); err != nil {
			return plan{}, err
		}
		if err := checkNoSymlink(workspace, tempRel(f.dest)); err != nil {
			return plan{}, err
		}
	}
	return p, nil
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
			p.files = append(p.files, plannedFile{src: path, dest: target, perm: info.Mode().Perm()})
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

// checkNoSymlink Lstats every existing component of workspace/rel, from the
// workspace down, and rejects the first symlink. Once a component is
// missing, nothing below it can exist.
func checkNoSymlink(workspace, rel string) error {
	cur := workspace
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("workspace: lstat %s: %w", cur, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrOutsideRoots, cur)
		}
	}
	return nil
}

// writer performs every write of one Apply call relative to a workspace
// directory descriptor, never following a symlink.
type writer struct {
	wsFD   int
	toSync map[string]bool // workspace-relative dirs to fsync at the end
}

func newWriter(workspace string) (*writer, error) {
	fd, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("workspace: open workspace %s: %w", workspace, err)
	}
	return &writer{wsFD: fd, toSync: map[string]bool{}}, nil
}

func (w *writer) close() { _ = unix.Close(w.wsFD) }

// openDir opens the directory at rel (workspace-relative, "." for the
// workspace) one component at a time with O_NOFOLLOW. With create set, a
// missing component is created and its parent is scheduled for fsync. The
// caller closes the returned descriptor.
func (w *writer) openDir(rel string, create bool) (int, error) {
	fd, err := unix.Dup(w.wsFD)
	if err != nil {
		return -1, fmt.Errorf("workspace: dup workspace fd: %w", err)
	}
	if rel == "." {
		return fd, nil
	}
	cur := "."
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		next, err := openDirAt(fd, part)
		if errors.Is(err, unix.ENOENT) && create {
			mkErr := unix.Mkdirat(fd, part, 0o755)
			switch {
			case mkErr == nil:
				w.toSync[cur] = true
			case !errors.Is(mkErr, unix.EEXIST):
				_ = unix.Close(fd)
				return -1, fmt.Errorf("workspace: create %s: %w", filepath.Join(cur, part), mkErr)
			}
			next, err = openDirAt(fd, part)
		}
		if err != nil {
			err = classifyOpenErr(fd, part, filepath.Join(cur, part), err)
			_ = unix.Close(fd)
			return -1, err
		}
		_ = unix.Close(fd)
		fd = next
		cur = filepath.Join(cur, part)
	}
	return fd, nil
}

func openDirAt(dirFD int, name string) (int, error) {
	return unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
}

// classifyOpenErr turns an O_NOFOLLOW refusal on a symlink into
// ErrOutsideRoots; anything else stays a plain error.
func classifyOpenErr(dirFD int, name, rel string, err error) error {
	var st unix.Stat_t
	if statErr := unix.Fstatat(dirFD, name, &st, unix.AT_SYMLINK_NOFOLLOW); statErr == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
		return fmt.Errorf("%w: %s is a symlink", ErrOutsideRoots, rel)
	}
	return fmt.Errorf("workspace: open dir %s: %w", rel, err)
}

func (w *writer) mkdirAll(rel string) error {
	fd, err := w.openDir(rel, true)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

// writeFile writes one file durably through its temp sibling.
func (w *writer) writeFile(f plannedFile) error {
	dirRel := filepath.Dir(f.dest)
	base := filepath.Base(f.dest)
	tmp := "." + base + tmpSuffix

	dirFD, err := w.openDir(dirRel, true)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirFD) }()

	src, err := openSource(f.src)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	// A leftover temp — from a crash, read-only, or a planted symlink — is
	// removed rather than opened: O_EXCL below then guarantees the file
	// written is one this call created.
	if err := unix.Unlinkat(dirFD, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("workspace: remove stale temp for %s: %w", f.dest, err)
	}
	tmpFD, err := unix.Openat(dirFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return fmt.Errorf("workspace: create temp file for %s: %w", f.dest, err)
	}
	out := os.NewFile(uintptr(tmpFD), filepath.Join(dirRel, tmp))

	fail := func(step string, err error) error {
		_ = out.Close()
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("workspace: %s %s: %w", step, f.dest, err)
	}
	if _, err := io.Copy(out, src); err != nil {
		return fail("copy", err)
	}
	// Through the handle: a mode change by name could follow a symlink.
	if err := out.Chmod(f.perm); err != nil {
		return fail("chmod temp file for", err)
	}
	if err := out.Sync(); err != nil {
		return fail("fsync temp file for", err)
	}
	if err := out.Close(); err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("workspace: close temp file for %s: %w", f.dest, err)
	}
	if err := unix.Renameat(dirFD, tmp, dirFD, base); err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("workspace: rename into place %s: %w", f.dest, err)
	}
	w.toSync[dirRel] = true
	return nil
}

// openSource opens one source file without following a symlink and without
// blocking on a FIFO, then insists it is a regular file.
func openSource(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, path)
	}
	if err != nil {
		// A transient cause (permissions, I/O) stays retryable.
		return nil, fmt.Errorf("workspace: open source %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, path)
	}
	return f, nil
}

// syncDirs fsyncs every scheduled directory, deepest first, making the
// renames and directory creations of this call durable.
func (w *writer) syncDirs() error {
	dirs := make([]string, 0, len(w.toSync))
	for d := range w.toSync {
		dirs = append(dirs, d)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		fd, err := w.openDir(d, false)
		if err != nil {
			return err
		}
		syncErr := unix.Fsync(fd)
		_ = unix.Close(fd)
		if syncErr != nil {
			return fmt.Errorf("workspace: fsync dir %s: %w", d, syncErr)
		}
		if syncedDirHook != nil {
			syncedDirHook(d)
		}
	}
	return nil
}
