// internal/personality/personality_test.go
package personality

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultTraits(t *testing.T) {
	d := DefaultTraits()
	if d.Thoroughness != 0.70 {
		t.Errorf("Thoroughness = %v, want 0.70", d.Thoroughness)
	}
	if d.ConfidenceThreshold != 0.78 {
		t.Errorf("ConfidenceThreshold = %v, want 0.78", d.ConfidenceThreshold)
	}
	if d.ScopeAppetite != 0.20 {
		t.Errorf("ScopeAppetite = %v, want 0.20", d.ScopeAppetite)
	}
	if d.PatternConformity != 0.80 {
		t.Errorf("PatternConformity = %v, want 0.80", d.PatternConformity)
	}
}

func TestTraitsClamp(t *testing.T) {
	tr := Traits{Thoroughness: 1.5, RiskTolerance: -0.3}
	clamped := tr.Clamp()
	if clamped.Thoroughness != 1.0 {
		t.Errorf("Thoroughness = %v, want 1.0", clamped.Thoroughness)
	}
	if clamped.RiskTolerance != 0.0 {
		t.Errorf("RiskTolerance = %v, want 0.0", clamped.RiskTolerance)
	}
}

func TestTraitsAdd(t *testing.T) {
	base := DefaultTraits()
	delta := Traits{Thoroughness: 0.1, RiskTolerance: -0.1}
	result := base.Add(delta)
	if result.Thoroughness != 0.80 {
		t.Errorf("Thoroughness = %v, want 0.80", result.Thoroughness)
	}
	if result.RiskTolerance != 0.20 {
		t.Errorf("RiskTolerance = %v, want 0.20", result.RiskTolerance)
	}
}

func TestLoadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "personality.yaml")
	content := []byte(`version: 1
name: "test"
description: "Test personality"
traits:
  thoroughness: 0.90
  context_hunger: 0.50
  confidence_threshold: 0.80
  pattern_recognition: 0.30
  risk_tolerance: 0.60
  scope_appetite: 0.20
  test_affinity: 0.40
  creativity: 0.20
  retry_persistence: 0.30
  strictness: 0.70
  pattern_conformity: 0.80
  documentation_drive: 0.20
  speed_vs_polish: 0.55
  verbosity: 0.35
  explanation_depth: 0.40
  notification_eagerness: 0.50
  defensiveness: 0.25
  tone: 0.60
  autonomy: 0.30
  collaboration: 0.70
  initiative: 0.30
  scope_sensitivity: 0.75
`)
	os.WriteFile(path, content, 0o644)

	pf, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if pf.Name != "test" {
		t.Errorf("Name = %q, want %q", pf.Name, "test")
	}
	if pf.Traits.Thoroughness != 0.90 {
		t.Errorf("Thoroughness = %v, want 0.90", pf.Traits.Thoroughness)
	}
}

func TestLoadFileMissing(t *testing.T) {
	pf, err := LoadFile("/nonexistent/path.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if pf.Name != "default" {
		t.Errorf("Name = %q, want %q", pf.Name, "default")
	}
	if pf.Traits != DefaultTraits() {
		t.Error("missing file should return default traits")
	}
}

func TestExport(t *testing.T) {
	pf := &PersonalityFile{
		Version:     1,
		Name:        "exported",
		Description: "Test export",
		Traits:      DefaultTraits(),
	}
	data, err := pf.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "name: exported") {
		t.Errorf("export should contain name, got:\n%s", data)
	}
}
