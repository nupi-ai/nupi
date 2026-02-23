package termresize

import "testing"

func TestNewStubMode(t *testing.T) {
	mode := NewStubMode("guided_fit")

	if got := mode.Name(); got != "guided_fit" {
		t.Fatalf("unexpected mode name: %s", got)
	}

	decision, err := mode.Handle(&Context{
		Request: ResizeRequest{SessionID: "session-1"},
	})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	if decision.SessionID != "session-1" {
		t.Fatalf("unexpected session id: %s", decision.SessionID)
	}
	if len(decision.Notes) != 1 || decision.Notes[0] != "guided_fit mode pending implementation" {
		t.Fatalf("unexpected notes: %+v", decision.Notes)
	}
}
