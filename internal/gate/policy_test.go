package gate

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func testRubric(t *testing.T) Rubric {
	t.Helper()
	r, err := LoadRubric(writeRubric(t, validRubric))
	require.NoError(t, err)
	return r
}

func verdict(t *testing.T, r Rubric, judge string, correctness, clarity int) Verdict {
	t.Helper()
	return Verdict{
		Judge:         judge,
		RubricVersion: r.Version,
		Scores:        map[string]int{"correctness": correctness, "clarity": clarity},
	}
}

func TestDecide_FailClosed(t *testing.T) {
	r := testRubric(t) // correctness: blocking, min 4 — clarity: advisory, min 3

	cases := []struct {
		name            string
		verdicts        func(t *testing.T) []Verdict
		outcome         Outcome
		failedBlocking  []string
		failedAdvisory  []string
	}{
		{
			name: "unanimous pass",
			verdicts: func(t *testing.T) []Verdict {
				return []Verdict{verdict(t, r, "haiku", 5, 4), verdict(t, r, "sonnet", 4, 3), verdict(t, r, "opus", 4, 5)}
			},
			outcome: OutcomePass,
		},
		{
			name: "single blocking dissent rejects",
			verdicts: func(t *testing.T) []Verdict {
				return []Verdict{verdict(t, r, "haiku", 5, 4), verdict(t, r, "sonnet", 3, 4), verdict(t, r, "opus", 5, 4)}
			},
			outcome:        OutcomeFail,
			failedBlocking: []string{"correctness"},
		},
		{
			name: "advisory majority fail is recorded but does not reject",
			verdicts: func(t *testing.T) []Verdict {
				return []Verdict{verdict(t, r, "haiku", 4, 2), verdict(t, r, "sonnet", 4, 2), verdict(t, r, "opus", 4, 4)}
			},
			outcome:        OutcomePass,
			failedAdvisory: []string{"clarity"},
		},
		{
			name: "advisory minority fail passes clean",
			verdicts: func(t *testing.T) []Verdict {
				return []Verdict{verdict(t, r, "haiku", 4, 2), verdict(t, r, "sonnet", 4, 4), verdict(t, r, "opus", 4, 4)}
			},
			outcome: OutcomePass,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Decide(r, tc.verdicts(t))
			require.NoError(t, err)
			require.Equal(t, tc.outcome, d.Outcome)
			require.Equal(t, tc.failedBlocking, d.FailedBlocking)
			require.Equal(t, tc.failedAdvisory, d.FailedAdvisory)
			require.Equal(t, PolicyFailClosedV1, d.Policy)
			require.Equal(t, r.Version, d.RubricVersion)
		})
	}
}

func TestDecide_IsDeterministic(t *testing.T) {
	r := testRubric(t)
	verdicts := []Verdict{
		verdict(t, r, "haiku", 3, 2),
		verdict(t, r, "sonnet", 5, 2),
		verdict(t, r, "opus", 4, 4),
	}

	first, err := Decide(r, verdicts)
	require.NoError(t, err)
	for i := 0; i < 100; i++ {
		again, err := Decide(r, verdicts)
		require.NoError(t, err)
		require.Equal(t, first, again,
			"replay must re-derive the identical decision from stored verdicts (ADR-0003)")
	}
}

func TestDecide_FailsClosedOnBadInput(t *testing.T) {
	r := testRubric(t)

	t.Run("no verdicts is an error, never a pass", func(t *testing.T) {
		_, err := Decide(r, nil)
		require.Error(t, err)
	})

	t.Run("rubric version mismatch is an error", func(t *testing.T) {
		v := verdict(t, r, "haiku", 5, 5)
		v.RubricVersion = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
		_, err := Decide(r, []Verdict{v})
		require.Error(t, err)
	})

	t.Run("missing axis score is an error, never a pass", func(t *testing.T) {
		v := Verdict{Judge: "haiku", RubricVersion: r.Version, Scores: map[string]int{"correctness": 5}}
		_, err := Decide(r, []Verdict{v})
		require.Error(t, err)
	})

	t.Run("score outside scale is an error", func(t *testing.T) {
		v := verdict(t, r, "haiku", 9, 3)
		_, err := Decide(r, []Verdict{v})
		require.Error(t, err)
	})
}
