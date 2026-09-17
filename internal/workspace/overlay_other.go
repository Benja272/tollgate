//go:build !unix

package workspace

import (
	"errors"
	"fmt"
)

// writer is unavailable without the Unix *at(2) syscall family: writing an
// overlay without O_NOFOLLOW-per-component could follow a symlink out of
// the declared roots.
type writer struct{}

func newWriter(string) (*writer, error) {
	return nil, fmt.Errorf("workspace: overlays need Unix *at(2) syscalls: %w", errors.ErrUnsupported)
}

func (*writer) close()                      {}
func (*writer) mkdirAll(string) error       { return errors.ErrUnsupported }
func (*writer) writeFile(plannedFile) error { return errors.ErrUnsupported }
func (*writer) syncDirs() error             { return errors.ErrUnsupported }
