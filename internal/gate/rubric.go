// Package gate implements the pre-PR quality gate: versioned rubrics, judge
// verdicts, and the resolution policy that turns N verdicts into one
// decision (ADR-0003).
package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Judges score each axis on this scale.
const (
	ScaleMin = 1
	ScaleMax = 5
)

// Axis is one dimension a judge scores. A blocking axis can reject the job;
// advisory axes only inform.
type Axis struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Blocking    bool   `yaml:"blocking"`
	MinScore    int    `yaml:"min_score"`
}

// Rubric is a versioned set of axes. Version is content-addressed: the
// sha256 of the rubric file bytes, so any edit yields a new version and a
// stored decision can pin exactly what it was judged against.
type Rubric struct {
	Name    string `yaml:"name"`
	Axes    []Axis `yaml:"axes"`
	Version string `yaml:"-"`
}

// LoadRubric reads, validates, and content-addresses a rubric file.
func LoadRubric(path string) (Rubric, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Rubric{}, fmt.Errorf("read rubric: %w", err)
	}

	var r Rubric
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return Rubric{}, fmt.Errorf("parse rubric: %w", err)
	}

	if len(r.Axes) == 0 {
		return Rubric{}, fmt.Errorf("rubric %q: at least one axis is required", r.Name)
	}
	seen := make(map[string]bool, len(r.Axes))
	for _, ax := range r.Axes {
		if ax.Name == "" {
			return Rubric{}, fmt.Errorf("rubric %q: axis with empty name", r.Name)
		}
		if seen[ax.Name] {
			return Rubric{}, fmt.Errorf("rubric %q: duplicate axis %q", r.Name, ax.Name)
		}
		seen[ax.Name] = true
		if ax.MinScore < ScaleMin || ax.MinScore > ScaleMax {
			return Rubric{}, fmt.Errorf("rubric %q: axis %q min_score %d outside scale [%d,%d]",
				r.Name, ax.Name, ax.MinScore, ScaleMin, ScaleMax)
		}
	}

	sum := sha256.Sum256(raw)
	r.Version = "sha256:" + hex.EncodeToString(sum[:])
	return r, nil
}
