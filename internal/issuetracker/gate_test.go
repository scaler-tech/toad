package issuetracker

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// mockTracker implements Tracker for gate tests.
type mockTracker struct {
	NoopTracker
	status     *IssueStatus
	statusErr  error
	commentErr error
	commented  bool
	commentRef *IssueRef // captures the ref passed to PostComment
}

func (m *mockTracker) GetIssueStatus(_ context.Context, _ *IssueRef) (*IssueStatus, error) {
	return m.status, m.statusErr
}

func (m *mockTracker) PostComment(_ context.Context, ref *IssueRef, _ string) error {
	m.commented = true
	m.commentRef = ref
	return m.commentErr
}

func TestCheckAssigneeGate_NilRef(t *testing.T) {
	gate := CheckAssigneeGate(context.Background(), &mockTracker{}, GateOpts{
		IssueRef: nil,
	})
	if gate.Gated {
		t.Error("expected not gated when issue ref is nil")
	}
}

func TestCheckAssigneeGate_StatusFetchFails(t *testing.T) {
	mt := &mockTracker{statusErr: fmt.Errorf("API error")}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
	})
	if gate.Gated {
		t.Error("expected fail-open when status fetch fails")
	}
}

func TestCheckAssigneeGate_NilStatus(t *testing.T) {
	mt := &mockTracker{status: nil}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
	})
	if gate.Gated {
		t.Error("expected not gated when status is nil")
	}
}

func TestCheckAssigneeGate_Unassigned(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:      "In Progress",
		InternalID: "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
	})
	if gate.Gated {
		t.Error("expected not gated when issue is unassigned")
	}
}

func TestCheckAssigneeGate_StaleAssignment(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "In Progress",
		AssigneeName: "Old Dev",
		AssignedAt:   time.Now().AddDate(0, 0, -30), // 30 days ago
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
	})
	if gate.Gated {
		t.Error("expected not gated when assignment is stale")
	}
}

func TestCheckAssigneeGate_ActiveAssignment(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "In Progress",
		AssigneeName: "Jane Doe",
		AssignedAt:   time.Now().AddDate(0, 0, -2), // 2 days ago
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:       &IssueRef{ID: "PLF-1"},
		StaleDays:      7,
		Findings:       "Fix the null check",
		SlackPermalink: "https://slack.com/thread/123",
	})
	if !gate.Gated {
		t.Fatal("expected gated when issue is actively assigned")
	}
	if gate.Status.AssigneeName != "Jane Doe" {
		t.Errorf("expected assignee Jane Doe, got %q", gate.Status.AssigneeName)
	}
	if !mt.commented {
		t.Error("expected comment to be posted")
	}
	// Gate should pass the pre-resolved InternalID to PostComment
	if mt.commentRef == nil || mt.commentRef.InternalID != "uuid-1" {
		t.Errorf("expected PostComment ref to carry InternalID 'uuid-1', got %+v", mt.commentRef)
	}
}

func TestCheckAssigneeGate_CommentFails_FailOpen(t *testing.T) {
	mt := &mockTracker{
		status: &IssueStatus{
			State:        "In Progress",
			AssigneeName: "Jane Doe",
			AssignedAt:   time.Now().AddDate(0, 0, -1),
			InternalID:   "uuid-1",
		},
		commentErr: fmt.Errorf("comment failed"),
	}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
		Findings:  "Fix it",
	})
	if gate.Gated {
		t.Error("expected fail-open when comment post fails")
	}
}

func TestCheckAssigneeGate_NoPermalink(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "In Progress",
		AssigneeName: "Jane Doe",
		AssignedAt:   time.Now(),
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
		Findings:  "Fix it",
		// No permalink — should still work
	})
	if !gate.Gated {
		t.Error("expected gated even without permalink")
	}
	if !mt.commented {
		t.Error("expected comment to be posted")
	}
}

func TestCheckAssigneeGate_DoneTicket(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "Done",
		AssigneeName: "Jane Doe",
		AssignedAt:   time.Now(),
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
		Findings:  "Fix something",
	})
	if !gate.Gated {
		t.Fatal("expected gated for Done ticket")
	}
	if !gate.Done {
		t.Error("expected Done=true for terminal state")
	}
	if mt.commented {
		t.Error("should NOT post a comment on Done tickets")
	}
}

func TestCheckAssigneeGate_CanceledTicket(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:      "Cancelled", //nolint:misspell // Linear uses British spelling
		InternalID: "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
	})
	if !gate.Gated {
		t.Fatal("expected gated for canceled ticket")
	}
	if !gate.Done {
		t.Error("expected Done=true for canceled state")
	}
}

func TestCheckAssigneeGate_DelegatedTicket(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "In Progress",
		DelegateName: "Biome",
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
		Findings:  "Fix something",
	})
	if !gate.Gated {
		t.Fatal("expected gated for delegated ticket")
	}
	if !gate.Delegated {
		t.Error("expected Delegated=true for a ticket with a delegate")
	}
	if gate.Done {
		t.Error("expected Done=false for a delegated (non-terminal) ticket")
	}
	if mt.commented {
		t.Error("should NOT post a comment on a delegated ticket — toad stays hands-off")
	}
}

// TestCheckAssigneeGate_DelegatedTakesPriorityOverAssignment covers a ticket
// that's both delegated AND actively assigned (e.g. delegated to an agent
// that Linear also surfaces as the assignee) — delegation must still win and
// stay silent, not fall into the assignee-gate comment path.
func TestCheckAssigneeGate_DelegatedTakesPriorityOverAssignment(t *testing.T) {
	mt := &mockTracker{status: &IssueStatus{
		State:        "In Progress",
		AssigneeName: "Jane Doe",
		AssignedAt:   time.Now(),
		DelegateName: "Biome",
		InternalID:   "uuid-1",
	}}
	gate := CheckAssigneeGate(context.Background(), mt, GateOpts{
		IssueRef:  &IssueRef{ID: "PLF-1"},
		StaleDays: 7,
		Findings:  "Fix something",
	})
	if !gate.Delegated {
		t.Error("expected Delegated=true even when an assignee is also present")
	}
	if mt.commented {
		t.Error("should NOT post a comment when delegated, regardless of assignment")
	}
}

func TestIsDelegated(t *testing.T) {
	tests := []struct {
		delegateName string
		want         bool
	}{
		{"Biome", true},
		{"", false},
	}
	for _, tt := range tests {
		s := &IssueStatus{DelegateName: tt.delegateName}
		if got := s.IsDelegated(); got != tt.want {
			t.Errorf("IsDelegated() for delegate %q = %v, want %v", tt.delegateName, got, tt.want)
		}
	}
}

func TestIsDone(t *testing.T) {
	tests := []struct {
		state string
		want  bool
	}{
		{"Done", true},
		{"done", true},
		{"DONE", true},
		{"Cancelled", true}, //nolint:misspell // Linear uses British spelling
		{"cancelled", true}, //nolint:misspell // Linear uses British spelling
		{"Canceled", true},
		{"Duplicate", true},
		{"duplicate", true},
		{"DUPLICATE", true},
		{"In Progress", false},
		{"Todo", false},
		{"", false},
	}
	for _, tt := range tests {
		s := &IssueStatus{State: tt.state}
		if got := s.IsDone(); got != tt.want {
			t.Errorf("IsDone() for state %q = %v, want %v", tt.state, got, tt.want)
		}
	}
}

func TestIsActivelyAssigned(t *testing.T) {
	tests := []struct {
		name      string
		status    IssueStatus
		staleDays int
		want      bool
	}{
		{
			name:      "no assignee",
			status:    IssueStatus{},
			staleDays: 7,
			want:      false,
		},
		{
			name:      "assignee with zero time",
			status:    IssueStatus{AssigneeName: "Jane"},
			staleDays: 7,
			want:      false,
		},
		{
			name:      "recent assignment",
			status:    IssueStatus{AssigneeName: "Jane", AssignedAt: time.Now().Add(-24 * time.Hour)},
			staleDays: 7,
			want:      true,
		},
		{
			name:      "stale assignment",
			status:    IssueStatus{AssigneeName: "Jane", AssignedAt: time.Now().Add(-10 * 24 * time.Hour)},
			staleDays: 7,
			want:      false,
		},
		{
			name:      "exactly at boundary",
			status:    IssueStatus{AssigneeName: "Jane", AssignedAt: time.Now().Add(-7*24*time.Hour + time.Hour)},
			staleDays: 7,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.status.IsActivelyAssigned(tt.staleDays)
			if got != tt.want {
				t.Errorf("IsActivelyAssigned(%d) = %v, want %v", tt.staleDays, got, tt.want)
			}
		})
	}
}
