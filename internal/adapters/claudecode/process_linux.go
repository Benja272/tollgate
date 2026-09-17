//go:build linux

package claudecode

import (
	"errors"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// configureProcess puts the CLI in its own process group and makes the
// kernel send deathSig to it when the worker dies, so an orphaned CLI never
// races the server's retry in the same workspace.
func configureProcess(cmd *exec.Cmd, deathSig syscall.Signal) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pdeathsig: deathSig}
}

// runProcess starts the CLI and waits for it. Once the group leader exits,
// and before it is reaped, the whole group is killed: a zombie leader keeps
// its pid — and therefore the group id — from being reused, so the kill can
// only reach processes the agent started.
func runProcess(cmd *exec.Cmd) error {
	// Pdeathsig fires when the OS thread that started the child exits, not
	// only the process; the thread stays locked until the child is reaped.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := cmd.Start(); err != nil {
		return err
	}
	waitLeaderExit(cmd.Process.Pid)
	_ = killGroup(cmd, syscall.SIGKILL)
	return cmd.Wait()
}

// waitLeaderExit blocks until pid exits, leaving it unreaped.
func waitLeaderExit(pid int) {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return
		}
	}
}
