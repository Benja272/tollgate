package workspace

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// engineFixture builds a workspace holding an "engine" file outside the
// declared root and an empty "output" root, the shape every frozen-engine
// test attacks.
func engineFixture(t *testing.T) (ws, engineFile string) {
	t.Helper()
	ws = t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "engine"), 0o755))
	engineFile = filepath.Join(ws, "engine", "main.go")
	writeFile(t, engineFile, "ENGINE", 0o644)
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))
	return ws, engineFile
}

func requireEngineUntouched(t *testing.T, engineFile string) {
	t.Helper()
	got, err := os.ReadFile(engineFile)
	require.NoError(t, err)
	require.Equal(t, "ENGINE", string(got), "engine code must never change")
	info, err := os.Stat(engineFile)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "engine file mode must never change")
}

// A symlink tracked at the pinned commit, BELOW a declared root, pointing
// at engine code inside the same workspace (review B1, reproduced).
func TestApply_SymlinkBelowRootInPinnedTree_EngineUntouchedAndRejected(t *testing.T) {
	ws, engineFile := engineFixture(t)
	require.NoError(t, os.Symlink("../engine", filepath.Join(ws, "output", "sub")))

	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	writeFile(t, filepath.Join(src, "sub", "main.go"), "HACKED", 0o755)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output"}}, func() {})

	require.ErrorIs(t, err, ErrOutsideRoots)
	requireEngineUntouched(t, engineFile)
}

// A symlink sitting at the temp name of a single-file overlay (review B2,
// reproduced): O_TRUNC through it rewrote the engine file and Chmod by name
// changed its mode.
func TestApply_SymlinkAtTempName_EngineUntouchedAndRejected(t *testing.T) {
	ws, engineFile := engineFixture(t)
	require.NoError(t, os.Symlink("../engine/main.go", filepath.Join(ws, "output", ".x.tollgate.tmp")))

	src := filepath.Join(t.TempDir(), "x")
	writeFile(t, src, "HACKED", 0o755)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, func() {})

	require.ErrorIs(t, err, ErrOutsideRoots)
	requireEngineUntouched(t, engineFile)
}

// plantAfterPrecheck makes the pre-check pass, then plants plant() before
// the first write: the write path itself must refuse to follow symlinks,
// not only the pre-check.
func plantAfterPrecheck(t *testing.T, plant func()) {
	t.Helper()
	afterPrecheckHook = plant
	t.Cleanup(func() { afterPrecheckHook = nil })
}

func TestApply_SymlinkDirPlantedAfterPrecheck_WriteRefusesToFollow(t *testing.T) {
	ws, engineFile := engineFixture(t)
	plantAfterPrecheck(t, func() {
		require.NoError(t, os.Symlink("../engine", filepath.Join(ws, "output", "sub")))
	})

	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	writeFile(t, filepath.Join(src, "sub", "main.go"), "HACKED", 0o644)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output"}}, nil)

	require.ErrorIs(t, err, ErrOutsideRoots)
	requireEngineUntouched(t, engineFile)
}

func TestApply_SymlinkTempPlantedAfterPrecheck_ReplacedNotFollowed(t *testing.T) {
	ws, engineFile := engineFixture(t)
	plantAfterPrecheck(t, func() {
		require.NoError(t, os.Symlink("../engine/main.go", filepath.Join(ws, "output", ".x.tollgate.tmp")))
	})

	src := filepath.Join(t.TempDir(), "x")
	writeFile(t, src, "payload", 0o755)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, nil)

	require.NoError(t, err)
	requireEngineUntouched(t, engineFile)
	got, readErr := os.ReadFile(filepath.Join(ws, "output", "x"))
	require.NoError(t, readErr)
	require.Equal(t, "payload", string(got))
	info, statErr := os.Lstat(filepath.Join(ws, "output", "x"))
	require.NoError(t, statErr)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

// A source holding both ".x.tollgate.tmp" and "x" used to lose one file
// silently: the second copy reused the first's path as its temp (review B3).
func TestApply_SourceEntryWithTempSuffix_RejectedNothingWritten(t *testing.T) {
	ws, _ := engineFixture(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, ".x.tollgate.tmp"), "one", 0o644)
	writeFile(t, filepath.Join(src, "x"), "two", 0o644)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/copied"}}, nil)

	require.ErrorIs(t, err, ErrUnsupportedSource)
	requireEmptyDir(t, filepath.Join(ws, "output"))
}

func TestApply_SourceTreeWithGitSegment_Rejected(t *testing.T) {
	for _, rel := range []string{".git/config", "sub/.git/config", ".GIT/config", "sub/.Git/hooks/x"} {
		t.Run(rel, func(t *testing.T) {
			ws, _ := engineFixture(t)
			src := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(src, rel)), 0o755))
			writeFile(t, filepath.Join(src, rel), "[core]\n\tfsmonitor = /bin/evil\n", 0o644)

			err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/copied"}}, nil)

			require.ErrorIs(t, err, ErrUnsupportedSource)
			requireEmptyDir(t, filepath.Join(ws, "output"))
		})
	}
}

func TestApply_SourceFIFO_Rejected(t *testing.T) {
	ws, _ := engineFixture(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "ok.txt"), "ok", 0o644)
	require.NoError(t, syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644))

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/copied"}}, nil)
	require.ErrorIs(t, err, ErrUnsupportedSource)

	err = Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: filepath.Join(src, "pipe"), Dest: "output/p"}}, nil)
	require.ErrorIs(t, err, ErrUnsupportedSource)
	requireEmptyDir(t, filepath.Join(ws, "output"))
}

func TestApply_FileOverlayOntoRoot_RejectedBeforeWriting(t *testing.T) {
	ws := t.TempDir()
	src := filepath.Join(t.TempDir(), "plan.md")
	writeFile(t, src, "plan", 0o644)

	err := Apply(context.Background(), ws, []string{"output/plan.md"}, []Overlay{{Source: src, Dest: "output/plan.md"}}, nil)

	require.ErrorIs(t, err, ErrOutsideRoots)
	_, statErr := os.Lstat(filepath.Join(ws, "output"))
	require.True(t, os.IsNotExist(statErr), "nothing may be created before the whole call is validated")
}

func TestApply_LeftoverReadOnlyTemp_DoesNotBlockRetry(t *testing.T) {
	ws, _ := engineFixture(t)
	writeFile(t, filepath.Join(ws, "output", ".x.tollgate.tmp"), "half-written", 0o400)

	src := filepath.Join(t.TempDir(), "x")
	writeFile(t, src, "complete", 0o444)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, nil)

	require.NoError(t, err)
	got, readErr := os.ReadFile(filepath.Join(ws, "output", "x"))
	require.NoError(t, readErr)
	require.Equal(t, "complete", string(got))
	info, statErr := os.Stat(filepath.Join(ws, "output", "x"))
	require.NoError(t, statErr)
	require.Equal(t, os.FileMode(0o444), info.Mode().Perm())
	matches, globErr := filepath.Glob(filepath.Join(ws, "output", ".*.tollgate.tmp"))
	require.NoError(t, globErr)
	require.Empty(t, matches)
}

// Every directory the overlay creates must be durable: its parent is
// fsynced, up to the first directory that already existed (review B4).
func TestApply_CreatedDirectories_ParentsFsynced(t *testing.T) {
	ws := t.TempDir()
	var mu sync.Mutex
	var synced []string
	syncedDirHook = func(rel string) {
		mu.Lock()
		defer mu.Unlock()
		synced = append(synced, rel)
	}
	t.Cleanup(func() { syncedDirHook = nil })

	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	writeFile(t, filepath.Join(src, "a", "b", "f.txt"), "f", 0o644)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/copied"}}, nil)
	require.NoError(t, err)

	sort.Strings(synced)
	require.Equal(t, []string{
		".",                 // parent of the created root "output"
		"output",            // parent of the created "copied"
		"output/copied",     // parent of the created "a"
		"output/copied/a",   // parent of the created "b"
		"output/copied/a/b", // holds the renamed f.txt
	}, synced)
}

func TestValidatePaths_TempSuffixAndCaseInsensitiveGit_Rejected(t *testing.T) {
	cases := map[string]struct {
		roots []string
		dest  string
	}{
		"dest ending in temp suffix": {[]string{"output"}, "output/.x.tollgate.tmp"},
		"root ending in temp suffix": {[]string{"output.tollgate.tmp"}, "output.tollgate.tmp/x"},
		"uppercase .GIT in dest":     {[]string{"output"}, "output/.GIT/config"},
		"mixed-case .Git in root":    {[]string{"output/.Git"}, "output/.Git/x"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := ValidatePaths(tc.roots, []Overlay{{Source: "/abs/src", Dest: tc.dest}})
			require.ErrorIs(t, err, ErrOutsideRoots)
		})
	}
}

func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "a rejected overlay must write nothing")
}

// An unreadable source is a transient condition, not an unsupported one:
// it must stay retryable (no non-retryable sentinel).
func TestApply_UnreadableSource_NotUnsupported(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission denial does not apply when running as root")
	}
	ws, _ := engineFixture(t)
	src := filepath.Join(t.TempDir(), "x")
	writeFile(t, src, "x", 0o000)

	err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, nil)

	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUnsupportedSource)
	require.NotErrorIs(t, err, ErrOutsideRoots)
}

// A directory created by a failed earlier attempt must still be made
// durable by the retry that succeeds (review R8, reproduced).
func TestApply_RetryAfterFailure_FsyncsParentsOfDirectoriesItDidNotCreate(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("permission denial does not apply when running as root")
	}
	ws := t.TempDir()
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "a"), 0o755))
	bad := filepath.Join(src, "a", "f.txt")
	writeFile(t, bad, "f", 0o000)
	ovs := []Overlay{{Source: src, Dest: "output/copied"}}

	require.Error(t, Apply(context.Background(), ws, []string{"output"}, ovs, nil), "first attempt fails after creating the directories")
	require.NoError(t, os.Chmod(bad, 0o644))

	var synced []string
	syncedDirHook = func(rel string) { synced = append(synced, rel) }
	t.Cleanup(func() { syncedDirHook = nil })
	require.NoError(t, Apply(context.Background(), ws, []string{"output"}, ovs, nil))

	for _, dir := range []string{".", "output", "output/copied", "output/copied/a"} {
		require.Contains(t, synced, dir)
	}
}

// Permanent destination conflicts must be classified, not retried.
func TestApply_DestinationConflicts_RejectedBeforeWriting(t *testing.T) {
	longName := strings.Repeat("n", 250) // fits a name, not its temp name
	cases := map[string]struct {
		setup func(t *testing.T, ws, src string) []Overlay
	}{
		"file overlay onto an existing directory": {func(t *testing.T, ws, src string) []Overlay {
			require.NoError(t, os.MkdirAll(filepath.Join(ws, "output", "x"), 0o755))
			return []Overlay{{Source: fileIn(t, src, "x"), Dest: "output/x"}}
		}},
		"path through an existing file": {func(t *testing.T, ws, src string) []Overlay {
			writeFile(t, filepath.Join(ws, "output", "x"), "tracked", 0o644)
			return []Overlay{{Source: fileIn(t, src, "y"), Dest: "output/x/y"}}
		}},
		"file and directory overlays collide": {func(t *testing.T, ws, src string) []Overlay {
			dir := filepath.Join(src, "tree")
			require.NoError(t, os.MkdirAll(dir, 0o755))
			writeFile(t, filepath.Join(dir, "y"), "y", 0o644)
			return []Overlay{
				{Source: fileIn(t, src, "x"), Dest: "output/x"},
				{Source: dir, Dest: "output/x"},
			}
		}},
		"file placed where another overlay needs a directory": {func(t *testing.T, ws, src string) []Overlay {
			return []Overlay{
				{Source: fileIn(t, src, "x"), Dest: "output/x"},
				{Source: fileIn(t, src, "y"), Dest: "output/x/y"},
			}
		}},
		"name too long for its temp name": {func(t *testing.T, ws, src string) []Overlay {
			return []Overlay{{Source: fileIn(t, src, "x"), Dest: "output/" + longName}}
		}},
		"directory at the temp name": {func(t *testing.T, ws, src string) []Overlay {
			require.NoError(t, os.MkdirAll(filepath.Join(ws, "output", ".x"+tmpSuffix), 0o755))
			return []Overlay{{Source: fileIn(t, src, "x"), Dest: "output/x"}}
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(ws, "output"), 0o755))
			ovs := tc.setup(t, ws, t.TempDir())
			before := treeSnapshot(t, ws)

			err := Apply(context.Background(), ws, []string{"output"}, ovs, nil)

			require.ErrorIs(t, err, ErrDestinationConflict)
			require.Equal(t, before, treeSnapshot(t, ws), "nothing may be written")
		})
	}
}

func fileIn(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	writeFile(t, p, name, 0o644)
	return p
}

func treeSnapshot(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel+":"+d.Type().String())
		return nil
	}))
	return out
}

// The write path must refuse a source swapped after the pre-check: removing
// O_NOFOLLOW or O_NONBLOCK must fail these tests.
func TestApply_SourceSwappedAfterPrecheck_Refused(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		ws, engineFile := engineFixture(t)
		src := filepath.Join(t.TempDir(), "x")
		writeFile(t, src, "x", 0o644)
		plantAfterPrecheck(t, func() {
			require.NoError(t, os.Remove(src))
			require.NoError(t, os.Symlink(engineFile, src))
		})

		err := Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, nil)

		require.ErrorIs(t, err, ErrUnsupportedSource)
		_, statErr := os.Lstat(filepath.Join(ws, "output", "x"))
		require.True(t, os.IsNotExist(statErr), "the symlink target must not be copied")
	})
	t.Run("fifo", func(t *testing.T) {
		ws, _ := engineFixture(t)
		src := filepath.Join(t.TempDir(), "x")
		writeFile(t, src, "x", 0o644)
		plantAfterPrecheck(t, func() {
			require.NoError(t, os.Remove(src))
			require.NoError(t, syscall.Mkfifo(src, 0o644))
		})

		done := make(chan error, 1)
		go func() {
			done <- Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/x"}}, nil)
		}()
		select {
		case err := <-done:
			require.ErrorIs(t, err, ErrUnsupportedSource)
		case <-time.After(3 * time.Second):
			t.Fatal("opening a FIFO source blocked")
		}
	})
}

func TestApply_EveryFileFsyncedBeforeRename(t *testing.T) {
	ws, _ := engineFixture(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "a", 0o644)
	writeFile(t, filepath.Join(src, "b"), "b", 0o644)
	var events []string
	syncedFileHook = func(rel string) { events = append(events, "fsync "+rel) }
	renamedFileHook = func(rel string) { events = append(events, "rename "+rel) }
	t.Cleanup(func() { syncedFileHook, renamedFileHook = nil, nil })

	require.NoError(t, Apply(context.Background(), ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/c"}}, nil))

	require.Equal(t, []string{
		"fsync output/c/a", "rename output/c/a",
		"fsync output/c/b", "rename output/c/b",
	}, events)
}

func TestApply_ContextCancelledBetweenFiles_StopsWriting(t *testing.T) {
	ws, _ := engineFixture(t)
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a"), "a", 0o644)
	writeFile(t, filepath.Join(src, "b"), "b", 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beats := 0
	beat := func() {
		beats++
		if beats == 2 {
			cancel()
		}
	}

	err := Apply(ctx, ws, []string{"output"}, []Overlay{{Source: src, Dest: "output/c"}}, beat)

	require.ErrorIs(t, err, context.Canceled)
	_, statErr := os.Stat(filepath.Join(ws, "output", "c", "b"))
	require.True(t, os.IsNotExist(statErr), "no file is written after cancellation")
}
