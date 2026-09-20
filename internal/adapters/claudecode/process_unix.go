//go:build unix

package claudecode

import (
	"os/exec"
	"syscall"
)

// killProcessGroup sends sig to the process group pgid, whether or not this
// process started it: a group recorded by a run whose worker was killed
// outlives the command that created it.
func killProcessGroup(pgid int, sig syscall.Signal) error {
	return syscall.Kill(-pgid, sig)
}

// killGroup sends sig to the command's whole process group.
func killGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, sig)
}
