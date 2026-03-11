package personality

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/scaler-tech/toad/internal/state"
)

const learnedCap = 0.35

type Manager struct {
	mu          sync.RWMutex
	base        Traits
	learned     Traits
	store       *Store
	learning    bool
	interpreter *Interpreter
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

// Reload re-reads learned deltas from the DB. Useful for read-only instances
// (e.g. status server) that share the SQLite DB via WAL with the daemon.
func (m *Manager) Reload() error {
	if m.store == nil {
		return nil
	}
	deltas, err := m.store.SumDeltas()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.learned = Traits{}
	for trait, delta := range deltas {
		m.learned.Set(trait, delta)
	}
	return nil
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

// SetInterpreter attaches an LLM interpreter for text-based feedback.
func (m *Manager) SetInterpreter(i *Interpreter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interpreter = i
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

	slog.Info("personality manual adjustment", "trait", trait, "value", targetValue, "delta", adjustDelta)

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

// ProcessText interprets free-text feedback using an LLM and applies trait adjustments.
func (m *Manager) ProcessText(ctx context.Context, text, threadKey string) error {
	if !m.LearningEnabled() {
		return nil
	}

	m.mu.RLock()
	interp := m.interpreter
	m.mu.RUnlock()

	if interp == nil {
		slog.Debug("personality text feedback skipped (no interpreter)")
		return nil
	}

	effective := m.Effective()
	adjustments, err := interp.Interpret(ctx, text, threadKey, effective)
	if err != nil {
		slog.Warn("personality text interpretation failed", "error", err)
		return nil // fail-open: skip adjustment on error
	}

	for _, adj := range adjustments {
		detail := fmt.Sprintf("text feedback: %s", truncate(text, 60))
		if err := m.applyAdjustment(adj.Trait, adj.Delta, "text", detail, adj.Reasoning); err != nil {
			slog.Error("personality text adjustment failed", "trait", adj.Trait, "error", err)
		}
	}

	if len(adjustments) > 0 {
		slog.Info("personality text feedback applied", "adjustments", len(adjustments), "thread", threadKey)
	}
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
	slog.Info("personality imported, clearing adjustment history", "name", pf.Name)
	m.base = pf.Traits
	m.learned = Traits{}
	if m.store != nil {
		return m.store.ClearAll()
	}
	return nil
}
