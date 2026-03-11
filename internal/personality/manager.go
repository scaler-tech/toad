package personality

import (
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/scaler-tech/toad/internal/state"
)

const learnedCap = 0.35

type Manager struct {
	mu       sync.RWMutex
	base     Traits
	learned  Traits
	store    *Store
	learning bool
}

func NewManager(base Traits) *Manager {
	return &Manager{base: base, learning: true}
}

func NewPersistentManager(db *state.DB, base Traits) (*Manager, error) {
	s := NewStore(db)
	deltas, err := s.SumDeltas()
	if err != nil {
		return nil, fmt.Errorf("hydrating personality: %w", err)
	}
	var learned Traits
	for trait, delta := range deltas {
		learned.Set(trait, delta)
	}
	return &Manager{base: base, learned: learned, store: s, learning: true}, nil
}

func (m *Manager) Effective() Traits {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.base.Add(m.learned).Clamp()
}

func (m *Manager) Base() Traits {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.base
}

func (m *Manager) LearningEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.learning
}

func (m *Manager) SetLearning(enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.learning = enabled
}

// ManualAdjust sets a trait to an absolute value by computing the required delta.
// Intentionally bypasses the learnedCap — manual dashboard edits are the escape
// hatch for exceeding the ±0.35 automatic learning cap (per spec).
func (m *Manager) ManualAdjust(trait string, targetValue float64, note string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	baseVal, ok := m.base.Get(trait)
	if !ok {
		return fmt.Errorf("unknown trait: %s", trait)
	}

	currentLearned, _ := m.learned.Get(trait)
	requiredDelta := targetValue - baseVal
	adjustDelta := requiredDelta - currentLearned

	m.learned.Set(trait, requiredDelta)

	if m.store != nil {
		return m.store.Insert(Adjustment{
			Trait:       trait,
			Delta:       adjustDelta,
			Source:      "manual",
			Reasoning:   note,
			BeforeValue: baseVal + currentLearned,
			AfterValue:  targetValue,
		})
	}
	return nil
}

func (m *Manager) applyAdjustment(trait string, rawDelta float64, source, triggerDetail, reasoning string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	baseVal, ok := m.base.Get(trait)
	if !ok {
		return fmt.Errorf("unknown trait: %s", trait)
	}

	currentLearned, _ := m.learned.Get(trait)
	currentEffective := clamp01(baseVal + currentLearned)

	// Dampening near extremes: max(0.05, 1 - |current - 0.5| * 2)
	dampen := math.Max(0.05, 1.0-math.Abs(currentEffective-0.5)*2.0)
	delta := rawDelta * dampen

	// Apply learned cap: ±0.35 from base
	newLearned := currentLearned + delta
	if newLearned > learnedCap {
		newLearned = learnedCap
	}
	if newLearned < -learnedCap {
		newLearned = -learnedCap
	}
	actualDelta := newLearned - currentLearned

	if actualDelta == 0 {
		return nil
	}

	m.learned.Set(trait, newLearned)
	newEffective := clamp01(baseVal + newLearned)

	if m.store != nil {
		return m.store.Insert(Adjustment{
			Trait:         trait,
			Delta:         actualDelta,
			Source:        source,
			TriggerDetail: triggerDetail,
			Reasoning:     reasoning,
			BeforeValue:   currentEffective,
			AfterValue:    newEffective,
		})
	}
	return nil
}

// ProcessText is a stub for LLM-interpreted feedback (deferred to follow-up plan).
func (m *Manager) ProcessText(text, context string) error {
	slog.Debug("personality text feedback received (not yet interpreted)", "text", text, "context", context)
	return nil
}

func (m *Manager) RecentAdjustments(limit int) ([]Adjustment, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.Recent(limit)
}

func (m *Manager) Export(name, description string) (*PersonalityFile, error) {
	eff := m.Effective()
	return &PersonalityFile{
		Version:     1,
		Name:        name,
		Description: description,
		Traits:      eff,
	}, nil
}

func (m *Manager) Import(pf *PersonalityFile) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.base = pf.Traits
	m.learned = Traits{}
	if m.store != nil {
		return m.store.ClearAll()
	}
	return nil
}
