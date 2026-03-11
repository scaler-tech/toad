package personality

import (
	"context"
	"time"

	"github.com/scaler-tech/toad/internal/state"
)

const storeTimeout = 10 * time.Second

type Adjustment struct {
	ID            int64
	Trait         string
	Delta         float64
	Source        string // "emoji", "llm_interpreted", "outcome", "manual"
	TriggerDetail string
	Reasoning     string
	BeforeValue   float64
	AfterValue    float64
	CreatedAt     time.Time
}

type Store struct {
	db *state.DB
}

func NewStore(db *state.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Insert(adj Adjustment) error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO personality_adjustments (trait, delta, source, trigger_detail, reasoning, before_value, after_value, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		adj.Trait, adj.Delta, adj.Source, adj.TriggerDetail, adj.Reasoning,
		adj.BeforeValue, adj.AfterValue, time.Now(),
	)
	return err
}

func (s *Store) SumDeltas() (map[string]float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		"SELECT trait, SUM(delta) FROM personality_adjustments GROUP BY trait")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	deltas := make(map[string]float64)
	for rows.Next() {
		var trait string
		var sum float64
		if err := rows.Scan(&trait, &sum); err != nil {
			return nil, err
		}
		deltas[trait] = sum
	}
	return deltas, rows.Err()
}

// Recent returns the N most recent adjustments, newest first.
// Uses ORDER BY id DESC for reliable ordering (timestamps may collide).
func (s *Store) Recent(limit int) ([]Adjustment, error) {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, trait, delta, source, COALESCE(trigger_detail,''), COALESCE(reasoning,''), before_value, after_value, created_at FROM personality_adjustments ORDER BY id DESC LIMIT ?",
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var adjs []Adjustment
	for rows.Next() {
		var a Adjustment
		if err := rows.Scan(&a.ID, &a.Trait, &a.Delta, &a.Source, &a.TriggerDetail, &a.Reasoning, &a.BeforeValue, &a.AfterValue, &a.CreatedAt); err != nil {
			return nil, err
		}
		adjs = append(adjs, a)
	}
	return adjs, rows.Err()
}

func (s *Store) ClearAll() error {
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx, "DELETE FROM personality_adjustments")
	return err
}
