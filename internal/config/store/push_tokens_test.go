package store

import (
	"testing"

	"github.com/nupi-ai/nupi/internal/constants"
)

func TestSavePushToken_RoundTrip(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	events := []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}
	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[abc123]", events, "auth-1"); err != nil {
		t.Fatalf("save push token: %v", err)
	}

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list push tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token, got %d", len(tokens))
	}

	pt := tokens[0]
	if pt.DeviceID != "device-1" {
		t.Errorf("device_id = %q, want %q", pt.DeviceID, "device-1")
	}
	if pt.Token != "ExponentPushToken[abc123]" {
		t.Errorf("token = %q, want %q", pt.Token, "ExponentPushToken[abc123]")
	}
	if len(pt.EnabledEvents) != 2 || pt.EnabledEvents[0] != constants.NotificationEventTaskCompleted || pt.EnabledEvents[1] != constants.NotificationEventError {
		t.Errorf("enabled_events = %v, want [TASK_COMPLETED ERROR]", pt.EnabledEvents)
	}
}

func TestSavePushToken_Upsert(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[old]", []string{constants.NotificationEventTaskCompleted}, "auth-1"); err != nil {
		t.Fatalf("save initial: %v", err)
	}

	// Update token and events for same device.
	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[new]", []string{constants.NotificationEventError, constants.NotificationEventInputNeeded}, "auth-1"); err != nil {
		t.Fatalf("save update: %v", err)
	}

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token after upsert, got %d", len(tokens))
	}
	if tokens[0].Token != "ExponentPushToken[new]" {
		t.Errorf("token = %q, want updated value", tokens[0].Token)
	}
	if len(tokens[0].EnabledEvents) != 2 {
		t.Errorf("enabled_events length = %d, want 2", len(tokens[0].EnabledEvents))
	}
}

func TestSavePushToken_Validation(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	tests := []struct {
		name     string
		deviceID string
		token    string
		events   []string
	}{
		{"empty device_id", "", "ExponentPushToken[x]", []string{constants.NotificationEventError}},
		{"whitespace device_id", "   ", "ExponentPushToken[x]", []string{constants.NotificationEventError}},
		{"empty token", "device-1", "", []string{constants.NotificationEventError}},
		{"whitespace token", "device-1", "   ", []string{constants.NotificationEventError}},
		{"empty events", "device-1", "ExponentPushToken[x]", []string{}},
		{"nil events", "device-1", "ExponentPushToken[x]", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := s.SavePushToken(ctx, tt.deviceID, tt.token, tt.events, "auth-1")
			if err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestGetPushToken_Found(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[abc123]", []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}, "auth-1"); err != nil {
		t.Fatalf("save push token: %v", err)
	}

	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get push token: %v", err)
	}
	if pt == nil {
		t.Fatal("expected push token, got nil")
	}
	if pt.DeviceID != "device-1" {
		t.Errorf("device_id = %q, want %q", pt.DeviceID, "device-1")
	}
	if pt.Token != "ExponentPushToken[abc123]" {
		t.Errorf("token = %q, want %q", pt.Token, "ExponentPushToken[abc123]")
	}
	if len(pt.EnabledEvents) != 2 || pt.EnabledEvents[0] != constants.NotificationEventTaskCompleted || pt.EnabledEvents[1] != constants.NotificationEventError {
		t.Errorf("enabled_events = %v, want [TASK_COMPLETED ERROR]", pt.EnabledEvents)
	}
	if pt.AuthTokenID != "auth-1" {
		t.Errorf("auth_token_id = %q, want %q", pt.AuthTokenID, "auth-1")
	}
}

func TestGetPushToken_NotFound(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	pt, err := s.GetPushToken(ctx, "non-existent")
	if err != nil {
		t.Fatalf("get push token: %v", err)
	}
	if pt != nil {
		t.Errorf("expected nil for non-existent device, got %+v", pt)
	}
}

func TestGetPushToken_EmptyDeviceID(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	_, err := s.GetPushToken(ctx, "")
	if err == nil {
		t.Error("expected error for empty device_id, got nil")
	}
}

func TestDeletePushToken(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[abc]", []string{constants.NotificationEventError}, "auth-1"); err != nil {
		t.Fatalf("save: %v", err)
	}

	if err := s.DeletePushToken(ctx, "device-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens after delete, got %d", len(tokens))
	}
}

func TestDeletePushToken_NotFound(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Deleting non-existent device should not error (idempotent).
	if err := s.DeletePushToken(ctx, "non-existent"); err != nil {
		t.Errorf("delete non-existent should not error: %v", err)
	}
}

func TestDeletePushToken_EmptyDeviceID(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.DeletePushToken(ctx, ""); err == nil {
		t.Error("expected error for empty device_id, got nil")
	}
}

func TestListPushTokensForEvent(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Device 1: TASK_COMPLETED + ERROR
	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[aaa]", []string{constants.NotificationEventTaskCompleted, constants.NotificationEventError}, "auth-1"); err != nil {
		t.Fatalf("save device-1: %v", err)
	}
	// Device 2: INPUT_NEEDED only
	if err := s.SavePushToken(ctx, "device-2", "ExponentPushToken[bbb]", []string{constants.NotificationEventInputNeeded}, "auth-2"); err != nil {
		t.Fatalf("save device-2: %v", err)
	}
	// Device 3: all events
	if err := s.SavePushToken(ctx, "device-3", "ExponentPushToken[ccc]", []string{constants.NotificationEventTaskCompleted, constants.NotificationEventInputNeeded, constants.NotificationEventError}, "auth-3"); err != nil {
		t.Fatalf("save device-3: %v", err)
	}

	// Filter by TASK_COMPLETED → device-1, device-3
	tokens, err := s.ListPushTokensForEvent(ctx, constants.NotificationEventTaskCompleted)
	if err != nil {
		t.Fatalf("list for TASK_COMPLETED: %v", err)
	}
	if len(tokens) != 2 {
		t.Errorf("TASK_COMPLETED: expected 2 tokens, got %d", len(tokens))
	}

	// Filter by INPUT_NEEDED → device-2, device-3
	tokens, err = s.ListPushTokensForEvent(ctx, constants.NotificationEventInputNeeded)
	if err != nil {
		t.Fatalf("list for INPUT_NEEDED: %v", err)
	}
	if len(tokens) != 2 {
		t.Errorf("INPUT_NEEDED: expected 2 tokens, got %d", len(tokens))
	}

	// Filter by ERROR → device-1, device-3
	tokens, err = s.ListPushTokensForEvent(ctx, constants.NotificationEventError)
	if err != nil {
		t.Fatalf("list for ERROR: %v", err)
	}
	if len(tokens) != 2 {
		t.Errorf("ERROR: expected 2 tokens, got %d", len(tokens))
	}

	// Filter by non-existent event → 0 tokens
	tokens, err = s.ListPushTokensForEvent(ctx, "UNKNOWN")
	if err != nil {
		t.Fatalf("list for UNKNOWN: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("UNKNOWN: expected 0 tokens, got %d", len(tokens))
	}
}

func TestListPushTokensForEvent_EmptyEventType(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	_, err := s.ListPushTokensForEvent(ctx, "")
	if err == nil {
		t.Error("expected error for empty event type, got nil")
	}
}

// L3/R9: verify that duplicate event types in enabled_events don't cause
// duplicate tokens in query results (json_each + EXISTS deduplicates).
func TestListPushTokensForEvent_DuplicateEventsInList(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Save token with duplicate constants.NotificationEventTaskCompleted entries.
	if err := s.SavePushToken(ctx, "device-dup", "ExponentPushToken[dup]", []string{constants.NotificationEventTaskCompleted, constants.NotificationEventTaskCompleted, constants.NotificationEventError}, "auth-1"); err != nil {
		t.Fatalf("save: %v", err)
	}

	tokens, err := s.ListPushTokensForEvent(ctx, constants.NotificationEventTaskCompleted)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Errorf("expected 1 token (no duplicates), got %d", len(tokens))
	}
}

func TestListPushTokens_Empty(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens on fresh store, got %d", len(tokens))
	}
}

func TestDeleteAllPushTokens(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.SavePushToken(ctx, "d1", "ExponentPushToken[a]", []string{constants.NotificationEventError}, "auth-1"); err != nil {
		t.Fatalf("save d1: %v", err)
	}
	if err := s.SavePushToken(ctx, "d2", "ExponentPushToken[b]", []string{constants.NotificationEventError}, "auth-2"); err != nil {
		t.Fatalf("save d2: %v", err)
	}

	if err := s.DeleteAllPushTokens(ctx); err != nil {
		t.Fatalf("delete all: %v", err)
	}

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens after delete all, got %d", len(tokens))
	}
}

func TestDeletePushTokensByAuthToken(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Two devices, different auth tokens.
	if err := s.SavePushToken(ctx, "d1", "ExponentPushToken[a]", []string{constants.NotificationEventError}, "auth-A"); err != nil {
		t.Fatalf("save d1: %v", err)
	}
	if err := s.SavePushToken(ctx, "d2", "ExponentPushToken[b]", []string{constants.NotificationEventError}, "auth-B"); err != nil {
		t.Fatalf("save d2: %v", err)
	}

	// Delete only auth-A's push tokens.
	if err := s.DeletePushTokensByAuthToken(ctx, "auth-A"); err != nil {
		t.Fatalf("delete by auth token: %v", err)
	}

	tokens, err := s.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token after scoped delete, got %d", len(tokens))
	}
	if tokens[0].DeviceID != "d2" {
		t.Errorf("remaining device = %q, want d2", tokens[0].DeviceID)
	}
}

func TestDeletePushTokensByAuthToken_EmptyID(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if err := s.DeletePushTokensByAuthToken(ctx, ""); err == nil {
		t.Error("expected error for empty auth_token_id, got nil")
	}
}

func TestSavePushToken_ReadOnly(t *testing.T) {
	t.Parallel()
	s, ctx := openReadOnlyTestStore(t)

	err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[x]", []string{constants.NotificationEventError}, "auth-1")
	if err == nil {
		t.Error("expected error for read-only store, got nil")
	}
}

// --- SavePushTokenOwned tests ---

func TestSavePushTokenOwned_FirstInsert(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	ok, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[abc]", []string{constants.NotificationEventError}, "auth-A")
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !ok {
		t.Fatal("expected true for first insert, got false")
	}

	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt.Token != "ExponentPushToken[abc]" {
		t.Errorf("token = %q, want ExponentPushToken[abc]", pt.Token)
	}
	if pt.AuthTokenID != "auth-A" {
		t.Errorf("auth_token_id = %q, want auth-A", pt.AuthTokenID)
	}
}

func TestSavePushTokenOwned_SameOwnerUpdate(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if _, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[old]", []string{constants.NotificationEventError}, "auth-A"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Same auth token updates successfully.
	ok, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[new]", []string{constants.NotificationEventTaskCompleted}, "auth-A")
	if err != nil {
		t.Fatalf("same-owner update: %v", err)
	}
	if !ok {
		t.Fatal("expected true for same-owner update, got false")
	}

	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt.Token != "ExponentPushToken[new]" {
		t.Errorf("token = %q, want ExponentPushToken[new]", pt.Token)
	}
}

func TestSavePushTokenOwned_DifferentOwnerDenied(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if _, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[owned]", []string{constants.NotificationEventError}, "auth-A"); err != nil {
		t.Fatalf("first insert: %v", err)
	}

	// Different auth token is denied.
	ok, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[hijack]", []string{constants.NotificationEventError}, "auth-B")
	if err != nil {
		t.Fatalf("different-owner update: %v", err)
	}
	if ok {
		t.Fatal("expected false for different-owner update, got true")
	}

	// Original token unchanged.
	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt.Token != "ExponentPushToken[owned]" {
		t.Errorf("token = %q, want ExponentPushToken[owned] (should be unchanged)", pt.Token)
	}
	if pt.AuthTokenID != "auth-A" {
		t.Errorf("auth_token_id = %q, want auth-A (should be unchanged)", pt.AuthTokenID)
	}
}

func TestSavePushTokenOwned_EmptyOwnerAllowsOverride(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Insert with empty auth_token_id via non-owned method.
	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[old]", []string{constants.NotificationEventError}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// SavePushTokenOwned with any auth token should succeed when existing has empty owner.
	ok, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[claimed]", []string{constants.NotificationEventTaskCompleted}, "auth-B")
	if err != nil {
		t.Fatalf("override empty owner: %v", err)
	}
	if !ok {
		t.Fatal("expected true when overriding empty-owner token, got false")
	}

	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt.AuthTokenID != "auth-B" {
		t.Errorf("auth_token_id = %q, want auth-B", pt.AuthTokenID)
	}
}

func TestSavePushTokenOwned_ReadOnly(t *testing.T) {
	t.Parallel()
	s, ctx := openReadOnlyTestStore(t)

	_, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[x]", []string{constants.NotificationEventError}, "auth-1")
	if err == nil {
		t.Error("expected error for read-only store, got nil")
	}
}

// --- DeletePushTokenOwned tests ---

func TestDeletePushTokenOwned_ByOwner(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if _, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[x]", []string{constants.NotificationEventError}, "auth-A"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	ok, err := s.DeletePushTokenOwned(ctx, "device-1", "auth-A")
	if err != nil {
		t.Fatalf("delete by owner: %v", err)
	}
	if !ok {
		t.Fatal("expected true for owner delete, got false")
	}

	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt != nil {
		t.Errorf("expected nil after delete, got %+v", pt)
	}
}

func TestDeletePushTokenOwned_DifferentOwnerDenied(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	if _, err := s.SavePushTokenOwned(ctx, "device-1", "ExponentPushToken[x]", []string{constants.NotificationEventError}, "auth-A"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Different owner cannot delete.
	ok, err := s.DeletePushTokenOwned(ctx, "device-1", "auth-B")
	if err != nil {
		t.Fatalf("delete by non-owner: %v", err)
	}
	if ok {
		t.Fatal("expected false for non-owner delete, got true")
	}

	// Token still exists.
	pt, err := s.GetPushToken(ctx, "device-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if pt == nil {
		t.Fatal("expected token to still exist after non-owner delete")
	}
}

func TestDeletePushTokenOwned_NotFound(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	ok, err := s.DeletePushTokenOwned(ctx, "non-existent", "auth-A")
	if err != nil {
		t.Fatalf("delete not-found: %v", err)
	}
	if ok {
		t.Fatal("expected false for non-existent device, got true")
	}
}

func TestDeletePushTokenOwned_EmptyOwnerAllowsDelete(t *testing.T) {
	t.Parallel()
	s, ctx := openTestStore(t)

	// Insert with empty auth_token_id.
	if err := s.SavePushToken(ctx, "device-1", "ExponentPushToken[x]", []string{constants.NotificationEventError}, ""); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Any auth token can delete when existing has empty owner.
	ok, err := s.DeletePushTokenOwned(ctx, "device-1", "auth-B")
	if err != nil {
		t.Fatalf("delete empty-owner: %v", err)
	}
	if !ok {
		t.Fatal("expected true when deleting empty-owner token, got false")
	}
}

func TestDeletePushTokenOwned_ReadOnly(t *testing.T) {
	t.Parallel()
	s, ctx := openReadOnlyTestStore(t)

	_, err := s.DeletePushTokenOwned(ctx, "device-1", "auth-1")
	if err == nil {
		t.Error("expected error for read-only store, got nil")
	}
}
