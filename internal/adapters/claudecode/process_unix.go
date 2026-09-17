//go:build unix

package claudecode

import (
	"os/exec"
	"syscall"
)

// killGroup sends sig to the command's whole process group.
func killGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
