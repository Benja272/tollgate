//go:build unix

package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

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
// missing component is created, and the parent of every component is
// scheduled for fsync. The caller closes the returned descriptor.
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
			if mkErr := unix.Mkdirat(fd, part, 0o755); mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
				_ = unix.Close(fd)
				return -1, asConflict(mkErr, "create %s", filepath.Join(cur, part))
			}
			next, err = openDirAt(fd, part)
		}
		if create && err == nil {
			// The entry may have been created by this call or by an earlier,
			// failed one; either way its parent is made durable.
			w.toSync[cur] = true
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

// conflictErrnos are the errors a write hits when the workspace changed
// shape after the pre-check; retrying cannot fix them.
var conflictErrnos = []error{unix.ENOTDIR, unix.EISDIR, unix.ENAMETOOLONG, unix.EEXIST, unix.ENOTEMPTY}

// asConflict marks err as ErrDestinationConflict when it is one of
// conflictErrnos.
func asConflict(err error, format string, args ...any) error {
	for _, errno := range conflictErrnos {
		if errors.Is(err, errno) {
			return fmt.Errorf("%w: %s: %v", ErrDestinationConflict, fmt.Sprintf(format, args...), err)
		}
	}
	return fmt.Errorf("workspace: %s: %w", fmt.Sprintf(format, args...), err)
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
	return asConflict(err, "open dir %s", rel)
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

	src, err := openSource(f)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	// A leftover temp — from a crash, read-only, or a planted symlink — is
	// removed rather than opened: O_EXCL below then guarantees the file
	// written is one this call created.
	if err := unix.Unlinkat(dirFD, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return asConflict(err, "remove stale temp for %s", f.dest)
	}
	tmpFD, err := unix.Openat(dirFD, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return asConflict(err, "create temp file for %s", f.dest)
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
	if err := fsyncFile(out, f.dest); err != nil {
		return fail("fsync temp file for", err)
	}
	if err := out.Close(); err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return fmt.Errorf("workspace: close temp file for %s: %w", f.dest, err)
	}
	if err := unix.Renameat(dirFD, tmp, dirFD, base); err != nil {
		_ = unix.Unlinkat(dirFD, tmp, 0)
		return asConflict(err, "rename into place %s", f.dest)
	}
	if renamedFileHook != nil {
		renamedFileHook(f.dest)
	}
	w.toSync[dirRel] = true
	return nil
}

// openSource opens one planned source without following a symlink and
// without blocking on a FIFO, then insists it is a regular file.
//
// O_NOFOLLOW protects the LAST component of a path only. A file planned
// from a source tree is therefore opened one component at a time below the
// tree's declared root, each with O_NOFOLLOW, so a directory inside the
// tree swapped for a symlink after the pre-check is refused instead of
// copying content from outside the tree. The components ABOVE the declared
// root are the caller's own path and are opened as given.
func openSource(f plannedFile) (*os.File, error) {
	if f.root == "" || f.rel == "" || f.rel == "." {
		return openSourceAt(unix.AT_FDCWD, f.src, f.src)
	}

	dirFD, err := unix.Open(f.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("workspace: open source tree %s: %w", f.root, err)
	}
	defer func() { _ = unix.Close(dirFD) }()

	parts := strings.Split(f.rel, string(filepath.Separator))
	for _, part := range parts[:len(parts)-1] {
		next, openErr := openDirAt(dirFD, part)
		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) || errors.Is(openErr, unix.ENOTDIR) {
				return nil, fmt.Errorf("%w: %s is no longer a directory in the source tree", ErrUnsupportedSource, f.src)
			}
			// A transient cause (permissions, I/O) stays retryable.
			return nil, fmt.Errorf("workspace: open source dir for %s: %w", f.src, openErr)
		}
		_ = unix.Close(dirFD)
		dirFD = next
	}
	return openSourceAt(dirFD, filepath.Base(f.rel), f.src)
}

// openSourceAt opens name relative to dirFD (or, with AT_FDCWD, by path)
// without following a symlink and without blocking on a FIFO, and insists
// the result is a regular file. path names the source in errors.
func openSourceAt(dirFD int, name, path string) (*os.File, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a symlink", ErrUnsupportedSource, path)
	}
	if err != nil {
		// A transient cause (permissions, I/O) stays retryable.
		return nil, fmt.Errorf("workspace: open source %s: %w", path, err)
	}
	src := os.NewFile(uintptr(fd), path)
	info, statErr := src.Stat()
	if statErr != nil || !info.Mode().IsRegular() {
		_ = src.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrUnsupportedSource, path)
	}
	return src, nil
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
		syncErr := fsyncDir(fd, d)
		_ = unix.Close(fd)
		if syncErr != nil {
			return fmt.Errorf("workspace: fsync dir %s: %w", d, syncErr)
		}
	}
	return nil
}

// fsyncDir makes the directory behind fd durable, and fsyncFile the file
// behind f. Each observation hook fires INSIDE the call that syncs, never
// after it: a test that watches durability must fail when the sync itself
// is gone, not only when the hook is.
func fsyncDir(fd int, rel string) error {
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if syncedDirHook != nil {
		syncedDirHook(rel)
	}
	return nil
}

func fsyncFile(f *os.File, rel string) error {
	if err := f.Sync(); err != nil {
		return err
	}
	if syncedFileHook != nil {
		syncedFileHook(rel)
	}
	return nil
}
