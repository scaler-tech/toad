package state

import (
	"errors"
	"testing"
)

func TestVacationState_TogglePersists(t *testing.T) {
	db := openTestDB(t)

	v := NewVacationState(db, false)
	if v.Active() {
		t.Error("expected vacation off by default")
	}

	if err := v.Set(true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if !v.Active() {
		t.Error("expected vacation on after Set(true)")
	}

	reloaded := NewVacationState(db, false)
	if !reloaded.Active() {
		t.Error("expected vacation state to persist across reload")
	}

	if err := reloaded.Set(false); err != nil {
		t.Fatalf("set: %v", err)
	}
	if NewVacationState(db, false).Active() {
		t.Error("expected vacation off after Set(false)")
	}
}

func TestVacationState_Forced(t *testing.T) {
	db := openTestDB(t)

	v := NewVacationState(db, true)
	if !v.Active() {
		t.Error("expected forced vacation to be active")
	}
	if !v.Forced() {
		t.Error("expected Forced() true")
	}
	if err := v.Set(false); !errors.Is(err, ErrVacationForced) {
		t.Errorf("expected ErrVacationForced, got %v", err)
	}
	if !v.Active() {
		t.Error("forced vacation must stay active")
	}
}

func TestVacationState_NilDB(t *testing.T) {
	v := NewVacationState(nil, false)
	if err := v.Set(true); err != nil {
		t.Fatalf("set without db: %v", err)
	}
	if !v.Active() {
		t.Error("expected in-memory toggle to work without db")
	}
}
