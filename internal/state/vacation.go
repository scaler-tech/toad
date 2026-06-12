package state

import (
	"errors"
	"sync/atomic"
)

const vacationSettingKey = "vacation_mode"

// ErrVacationForced is returned when trying to disable vacation mode that is
// forced on by config.
var ErrVacationForced = errors.New("vacation mode is forced on by config")

// VacationState tracks whether toad is on vacation. The state is persisted
// in the settings table so it survives restarts. When forced via config,
// it cannot be disabled at runtime.
type VacationState struct {
	forced bool
	active atomic.Bool
	db     *DB
}

// NewVacationState loads the persisted vacation state. The forced flag
// (from config) takes precedence over the stored setting.
func NewVacationState(db *DB, forced bool) *VacationState {
	v := &VacationState{forced: forced, db: db}
	if forced {
		v.active.Store(true)
		return v
	}
	if db != nil {
		if val, err := db.GetSetting(vacationSettingKey); err == nil && (val == "true" || val == "1") {
			v.active.Store(true)
		}
	}
	return v
}

// Active reports whether vacation mode is currently on.
func (v *VacationState) Active() bool {
	return v.active.Load()
}

// Forced reports whether vacation mode is locked on by config.
func (v *VacationState) Forced() bool {
	return v.forced
}

// Set toggles vacation mode and persists the new state.
func (v *VacationState) Set(on bool) error {
	if v.forced && !on {
		return ErrVacationForced
	}
	v.active.Store(on)
	if v.db == nil {
		return nil
	}
	val := "false"
	if on {
		val = "true"
	}
	return v.db.SetSetting(vacationSettingKey, val)
}
