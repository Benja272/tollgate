package ports

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunError_ErrorIncludesUnderlyingMessage(t *testing.T) {
	underlying := errors.New("agent reported is_error=true")
	re := &RunError{
		Result: RunResult{CostUSD: 1.25, Model: "claude-sonnet-5"},
		Err:    underlying,
	}

	assert.Contains(t, re.Error(), "agent reported is_error=true")
}

func TestRunError_UnwrapsToUnderlyingErr(t *testing.T) {
	sentinel := errors.New("sentinel failure")
	re := &RunError{
		Result: RunResult{CostUSD: 0.5, Model: "claude-haiku-5"},
		Err:    sentinel,
	}

	require.ErrorIs(t, re, sentinel)

	var target *RunError
	require.ErrorAs(t, re, &target)
	assert.Equal(t, "claude-haiku-5", target.Result.Model)
}

func TestUnmeteredRunError_UnwrapsAndNamesTheRisk(t *testing.T) {
	cause := errors.New("signal: killed")
	err := error(&UnmeteredRunError{Err: cause})

	require.ErrorIs(t, err, cause)
	require.Contains(t, err.Error(), "unmetered")
}
