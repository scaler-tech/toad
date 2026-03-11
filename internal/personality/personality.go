// Package personality implements a 22-trait behavioral personality system
// with dampened learning, YAML import/export, and mode-specific translation.
package personality

import (
	"fmt"
	"math"
	"os"

	"gopkg.in/yaml.v3"
)

// Traits holds all 22 personality trait values (0.0–1.0).
type Traits struct {
	// Investigation & Analysis
	Thoroughness        float64 `yaml:"thoroughness" json:"thoroughness"`
	ContextHunger       float64 `yaml:"context_hunger" json:"context_hunger"`
	ConfidenceThreshold float64 `yaml:"confidence_threshold" json:"confidence_threshold"`
	PatternRecognition  float64 `yaml:"pattern_recognition" json:"pattern_recognition"`

	// Action & Execution
	RiskTolerance    float64 `yaml:"risk_tolerance" json:"risk_tolerance"`
	ScopeAppetite    float64 `yaml:"scope_appetite" json:"scope_appetite"`
	TestAffinity     float64 `yaml:"test_affinity" json:"test_affinity"`
	Creativity       float64 `yaml:"creativity" json:"creativity"`
	RetryPersistence float64 `yaml:"retry_persistence" json:"retry_persistence"`

	// Quality & Standards
	Strictness         float64 `yaml:"strictness" json:"strictness"`
	PatternConformity  float64 `yaml:"pattern_conformity" json:"pattern_conformity"`
	DocumentationDrive float64 `yaml:"documentation_drive" json:"documentation_drive"`
	SpeedVsPolish      float64 `yaml:"speed_vs_polish" json:"speed_vs_polish"`

	// Communication
	Verbosity             float64 `yaml:"verbosity" json:"verbosity"`
	ExplanationDepth      float64 `yaml:"explanation_depth" json:"explanation_depth"`
	NotificationEagerness float64 `yaml:"notification_eagerness" json:"notification_eagerness"`
	Defensiveness         float64 `yaml:"defensiveness" json:"defensiveness"`
	Tone                  float64 `yaml:"tone" json:"tone"`

	// Autonomy & Initiative
	Autonomy         float64 `yaml:"autonomy" json:"autonomy"`
	Collaboration    float64 `yaml:"collaboration" json:"collaboration"`
	Initiative       float64 `yaml:"initiative" json:"initiative"`
	ScopeSensitivity float64 `yaml:"scope_sensitivity" json:"scope_sensitivity"`
}

func DefaultTraits() Traits {
	return Traits{
		Thoroughness:          0.70,
		ContextHunger:         0.50,
		ConfidenceThreshold:   0.80,
		PatternRecognition:    0.30,
		RiskTolerance:         0.30,
		ScopeAppetite:         0.20,
		TestAffinity:          0.40,
		Creativity:            0.20,
		RetryPersistence:      0.30,
		Strictness:            0.70,
		PatternConformity:     0.80,
		DocumentationDrive:    0.20,
		SpeedVsPolish:         0.55,
		Verbosity:             0.35,
		ExplanationDepth:      0.40,
		NotificationEagerness: 0.50,
		Defensiveness:         0.25,
		Tone:                  0.60,
		Autonomy:              0.30,
		Collaboration:         0.70,
		Initiative:            0.30,
		ScopeSensitivity:      0.75,
	}
}

// round rounds a float64 to a fixed number of decimal places to avoid
// floating-point accumulation errors when adding trait deltas.
func round(v float64) float64 {
	return math.Round(v*1e9) / 1e9
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func (t Traits) Clamp() Traits {
	return Traits{
		Thoroughness:          clamp01(t.Thoroughness),
		ContextHunger:         clamp01(t.ContextHunger),
		ConfidenceThreshold:   clamp01(t.ConfidenceThreshold),
		PatternRecognition:    clamp01(t.PatternRecognition),
		RiskTolerance:         clamp01(t.RiskTolerance),
		ScopeAppetite:         clamp01(t.ScopeAppetite),
		TestAffinity:          clamp01(t.TestAffinity),
		Creativity:            clamp01(t.Creativity),
		RetryPersistence:      clamp01(t.RetryPersistence),
		Strictness:            clamp01(t.Strictness),
		PatternConformity:     clamp01(t.PatternConformity),
		DocumentationDrive:    clamp01(t.DocumentationDrive),
		SpeedVsPolish:         clamp01(t.SpeedVsPolish),
		Verbosity:             clamp01(t.Verbosity),
		ExplanationDepth:      clamp01(t.ExplanationDepth),
		NotificationEagerness: clamp01(t.NotificationEagerness),
		Defensiveness:         clamp01(t.Defensiveness),
		Tone:                  clamp01(t.Tone),
		Autonomy:              clamp01(t.Autonomy),
		Collaboration:         clamp01(t.Collaboration),
		Initiative:            clamp01(t.Initiative),
		ScopeSensitivity:      clamp01(t.ScopeSensitivity),
	}
}

func (t Traits) Add(other Traits) Traits {
	return Traits{
		Thoroughness:          round(t.Thoroughness + other.Thoroughness),
		ContextHunger:         round(t.ContextHunger + other.ContextHunger),
		ConfidenceThreshold:   round(t.ConfidenceThreshold + other.ConfidenceThreshold),
		PatternRecognition:    round(t.PatternRecognition + other.PatternRecognition),
		RiskTolerance:         round(t.RiskTolerance + other.RiskTolerance),
		ScopeAppetite:         round(t.ScopeAppetite + other.ScopeAppetite),
		TestAffinity:          round(t.TestAffinity + other.TestAffinity),
		Creativity:            round(t.Creativity + other.Creativity),
		RetryPersistence:      round(t.RetryPersistence + other.RetryPersistence),
		Strictness:            round(t.Strictness + other.Strictness),
		PatternConformity:     round(t.PatternConformity + other.PatternConformity),
		DocumentationDrive:    round(t.DocumentationDrive + other.DocumentationDrive),
		SpeedVsPolish:         round(t.SpeedVsPolish + other.SpeedVsPolish),
		Verbosity:             round(t.Verbosity + other.Verbosity),
		ExplanationDepth:      round(t.ExplanationDepth + other.ExplanationDepth),
		NotificationEagerness: round(t.NotificationEagerness + other.NotificationEagerness),
		Defensiveness:         round(t.Defensiveness + other.Defensiveness),
		Tone:                  round(t.Tone + other.Tone),
		Autonomy:              round(t.Autonomy + other.Autonomy),
		Collaboration:         round(t.Collaboration + other.Collaboration),
		Initiative:            round(t.Initiative + other.Initiative),
		ScopeSensitivity:      round(t.ScopeSensitivity + other.ScopeSensitivity),
	}
}

func TraitNames() []string {
	return []string{
		"thoroughness", "context_hunger", "confidence_threshold", "pattern_recognition",
		"risk_tolerance", "scope_appetite", "test_affinity", "creativity", "retry_persistence",
		"strictness", "pattern_conformity", "documentation_drive", "speed_vs_polish",
		"verbosity", "explanation_depth", "notification_eagerness", "defensiveness", "tone",
		"autonomy", "collaboration", "initiative", "scope_sensitivity",
	}
}

func (t Traits) Get(name string) (float64, bool) {
	switch name {
	case "thoroughness":
		return t.Thoroughness, true
	case "context_hunger":
		return t.ContextHunger, true
	case "confidence_threshold":
		return t.ConfidenceThreshold, true
	case "pattern_recognition":
		return t.PatternRecognition, true
	case "risk_tolerance":
		return t.RiskTolerance, true
	case "scope_appetite":
		return t.ScopeAppetite, true
	case "test_affinity":
		return t.TestAffinity, true
	case "creativity":
		return t.Creativity, true
	case "retry_persistence":
		return t.RetryPersistence, true
	case "strictness":
		return t.Strictness, true
	case "pattern_conformity":
		return t.PatternConformity, true
	case "documentation_drive":
		return t.DocumentationDrive, true
	case "speed_vs_polish":
		return t.SpeedVsPolish, true
	case "verbosity":
		return t.Verbosity, true
	case "explanation_depth":
		return t.ExplanationDepth, true
	case "notification_eagerness":
		return t.NotificationEagerness, true
	case "defensiveness":
		return t.Defensiveness, true
	case "tone":
		return t.Tone, true
	case "autonomy":
		return t.Autonomy, true
	case "collaboration":
		return t.Collaboration, true
	case "initiative":
		return t.Initiative, true
	case "scope_sensitivity":
		return t.ScopeSensitivity, true
	default:
		return 0, false
	}
}

// PersonalityFile represents the structure of a personality YAML file.
type PersonalityFile struct {
	Version     int    `yaml:"version"`
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Traits      Traits `yaml:"traits"`
}

// LoadFile reads a personality YAML file from path. If the file does not exist,
// it returns a PersonalityFile with default traits rather than an error.
func LoadFile(path string) (*PersonalityFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &PersonalityFile{
				Version:     1,
				Name:        "default",
				Description: "Conservative, scope-disciplined, pattern-following. Toad's original personality.",
				Traits:      DefaultTraits(),
			}, nil
		}
		return nil, fmt.Errorf("reading personality file: %w", err)
	}
	var pf PersonalityFile
	if err := yaml.Unmarshal(data, &pf); err != nil {
		return nil, fmt.Errorf("parsing personality file: %w", err)
	}
	return &pf, nil
}

// Marshal serializes the PersonalityFile to YAML bytes.
func (pf *PersonalityFile) Marshal() ([]byte, error) {
	return yaml.Marshal(pf)
}

func (t *Traits) Set(name string, value float64) bool {
	switch name {
	case "thoroughness":
		t.Thoroughness = value
	case "context_hunger":
		t.ContextHunger = value
	case "confidence_threshold":
		t.ConfidenceThreshold = value
	case "pattern_recognition":
		t.PatternRecognition = value
	case "risk_tolerance":
		t.RiskTolerance = value
	case "scope_appetite":
		t.ScopeAppetite = value
	case "test_affinity":
		t.TestAffinity = value
	case "creativity":
		t.Creativity = value
	case "retry_persistence":
		t.RetryPersistence = value
	case "strictness":
		t.Strictness = value
	case "pattern_conformity":
		t.PatternConformity = value
	case "documentation_drive":
		t.DocumentationDrive = value
	case "speed_vs_polish":
		t.SpeedVsPolish = value
	case "verbosity":
		t.Verbosity = value
	case "explanation_depth":
		t.ExplanationDepth = value
	case "notification_eagerness":
		t.NotificationEagerness = value
	case "defensiveness":
		t.Defensiveness = value
	case "tone":
		t.Tone = value
	case "autonomy":
		t.Autonomy = value
	case "collaboration":
		t.Collaboration = value
	case "initiative":
		t.Initiative = value
	case "scope_sensitivity":
		t.ScopeSensitivity = value
	default:
		return false
	}
	return true
}
