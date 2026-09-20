package claudecode

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// The CLI must die with the worker, or it races the server's retry in the
// same workspace.
func TestConfigureProcess_SetsGroupAndParentDeathSignal(t *testing.T) {
	cmd := exec.Command("true")
	configureProcess(cmd, syscall.SIGKILL)

	require.NotNil(t, cmd.SysProcAttr)
	require.True(t, cmd.SysProcAttr.Setpgid)
	require.Equal(t, syscall.SIGKILL, cmd.SysProcAttr.Pdeathsig)
}
