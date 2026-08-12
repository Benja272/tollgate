package gate

import "fmt"

// Outcome is the gate's verdict over a judged attempt.
type Outcome string

const (
	OutcomePass Outcome = "pass"
	OutcomeFail Outcome = "fail"
)

// PolicyFailClosedV1 is the shipped default resolution policy: unanimous
// pass on blocking axes, majority on advisory axes (ADR-0003). The
// identifier is pinned into every decision so replay knows which rules
// derived it.
const PolicyFailClosedV1 = "fail-closed/v1"

// Verdict is one judge's scoring of one attempt against one rubric version.
type Verdict struct {
	Judge         string
	RubricVersion string
	Scores        map[string]int
	Findings      []string
}

// Decision is the resolution-policy output. It pins rubric and policy
// versions so it can be re-derived from the stored verdicts bit-for-bit.
type Decision struct {
	Outcome        Outcome
	Policy         string
	RubricVersion  string
	FailedBlocking []string
	FailedAdvisory []string
}

// Decide applies the fail-closed policy to a set of verdicts. It is a pure
// function: replaying it over stored verdicts must reproduce the stored
// decision exactly, and it never re-invokes a judge. Malformed input
// (missing verdicts, version mismatches, missing or out-of-scale scores) is
// an error, never a pass — the gate fails closed on broken machinery too.
func Decide(r Rubric, verdicts []Verdict) (Decision, error) {
	if len(verdicts) == 0 {
		return Decision{}, fmt.Errorf("decide: no verdicts for rubric %s", r.Version)
	}
	for _, v := range verdicts {
		if v.RubricVersion != r.Version {
			return Decision{}, fmt.Errorf("decide: verdict from %q pins rubric %s, want %s",
				v.Judge, v.RubricVersion, r.Version)
		}
		for _, ax := range r.Axes {
			score, ok := v.Scores[ax.Name]
			if !ok {
				return Decision{}, fmt.Errorf("decide: verdict from %q missing axis %q", v.Judge, ax.Name)
			}
			if score < ScaleMin || score > ScaleMax {
				return Decision{}, fmt.Errorf("decide: verdict from %q scores axis %q at %d, outside scale [%d,%d]",
					v.Judge, ax.Name, score, ScaleMin, ScaleMax)
			}
		}
	}

	d := Decision{
		Outcome:       OutcomePass,
		Policy:        PolicyFailClosedV1,
		RubricVersion: r.Version,
	}
	// Iterate axes in rubric order, never over score maps: Go map iteration
	// is randomized and would break bit-for-bit replay of FailedBlocking /
	// FailedAdvisory ordering.
	for _, ax := range r.Axes {
		passes := 0
		for _, v := range verdicts {
			if v.Scores[ax.Name] >= ax.MinScore {
				passes++
			}
		}
		if ax.Blocking {
			if passes < len(verdicts) {
				d.FailedBlocking = append(d.FailedBlocking, ax.Name)
				d.Outcome = OutcomeFail
			}
		} else if passes*2 <= len(verdicts) {
			d.FailedAdvisory = append(d.FailedAdvisory, ax.Name)
		}
	}
	return d, nil
}
