//go:build !unix

package claudecode

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

// The runner depends on Unix process groups; elsewhere it refuses to start
// the CLI rather than run it without a way to kill what it leaves behind.

func configureProcess(*exec.Cmd, syscall.Signal) {}

func killGroup(*exec.Cmd, syscall.Signal) error { return nil }

func runProcess(*exec.Cmd) error {
	return fmt.Errorf("claude code runner needs Unix process groups: %w", errors.ErrUnsupported)
}
