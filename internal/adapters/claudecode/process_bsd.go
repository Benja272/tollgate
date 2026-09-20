//go:build unix && !linux

package claudecode

import (
	"os/exec"
	"syscall"
)

// configureProcess puts the CLI in its own process group. There is no
// parent-death signal outside Linux.
func configureProcess(cmd *exec.Cmd, _ syscall.Signal) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// runProcess starts the CLI, calls started with the leader's pid — its
// process group id — waits for it, then kills its group. The leader is
// already reaped at that point, so its group id could in principle have
// been reused (ADR-0006 §8 records this residual).
func runProcess(cmd *exec.Cmd, started func(pid int)) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	started(cmd.Process.Pid)
	err := cmd.Wait()
	_ = killGroup(cmd, syscall.SIGKILL)
	return err
}
