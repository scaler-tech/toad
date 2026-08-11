package cmd

import (
	"net/http/httptest"
	"testing"
)

func TestConnectCallback_DeliversCode(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?code=abc123&state=expected", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	select {
	case got := <-codeCh:
		if got != "abc123" {
			t.Errorf("code = %q", got)
		}
	default:
		t.Fatal("code not delivered")
	}
}

func TestConnectCallback_RejectsStateMismatch(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?code=abc123&state=WRONG", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if len(codeCh) != 0 {
		t.Error("code must not be delivered on state mismatch")
	}
}

func TestConnectCallback_RejectsMissingCode(t *testing.T) {
	codeCh := make(chan string, 1)
	req := httptest.NewRequest("GET", "/callback?state=expected", nil)
	w := httptest.NewRecorder()
	runConnectCallback(w, req, "expected", codeCh)
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
