package server

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRegisterPushToken_Success(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := authCtxWithToken(t, apiServer, service)

	resp, err := service.RegisterPushToken(ctx, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_TASK_COMPLETED,
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("RegisterPushToken returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestRegisterPushToken_Unauthenticated(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for unauthenticated register")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_EmptyToken(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for empty token")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_InvalidTokenFormat(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "not-a-valid-token",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid token format")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_EmptyDeviceID(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: "",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for empty device_id")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_NoEvents(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:         "ExponentPushToken[test_abc12345]",
		DeviceId:      "device-1",
		EnabledEvents: []apiv1.NotificationEventType{},
	})
	if err == nil {
		t.Fatal("expected error for empty enabled_events")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_UnspecifiedEvent(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_UNSPECIFIED,
		},
	})
	if err == nil {
		t.Fatal("expected error for UNSPECIFIED event type")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_UnknownEventType(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType(99),
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown event type")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestRegisterPushToken_Upsert(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := authCtxWithToken(t, apiServer, service)

	// Register initial token.
	_, err := service.RegisterPushToken(ctx, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[old_token_123]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("first register: %v", err)
	}

	// Update with new token and events for same device.
	_, err = service.RegisterPushToken(ctx, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[new_token_456]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_TASK_COMPLETED,
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_INPUT_NEEDED,
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Verify via store that only one token exists for this device.
	tokens, err := apiServer.configStore.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token after upsert, got %d", len(tokens))
	}
	if tokens[0].Token != "ExponentPushToken[new_token_456]" {
		t.Errorf("token = %q, want ExponentPushToken[new_token_456]", tokens[0].Token)
	}
}

func TestUnregisterPushToken_Success(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := authCtxWithToken(t, apiServer, service)

	// Register first.
	_, err := service.RegisterPushToken(ctx, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc_def]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// Unregister.
	resp, err := service.UnregisterPushToken(ctx, &apiv1.UnregisterPushTokenRequest{
		DeviceId: "device-1",
	})
	if err != nil {
		t.Fatalf("UnregisterPushToken returned error: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify token was removed.
	tokens, err := apiServer.configStore.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("expected 0 tokens after unregister, got %d", len(tokens))
	}
}

func TestUnregisterPushToken_Unauthenticated(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.UnregisterPushToken(context.Background(), &apiv1.UnregisterPushTokenRequest{
		DeviceId: "device-1",
	})
	if err == nil {
		t.Fatal("expected error for unauthenticated unregister")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestUnregisterPushToken_EmptyDeviceID(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	_, err := service.UnregisterPushToken(context.Background(), &apiv1.UnregisterPushTokenRequest{
		DeviceId: "",
	})
	if err == nil {
		t.Fatal("expected error for empty device_id")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestUnregisterPushToken_NotFound(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := authCtxWithToken(t, apiServer, service)

	// Deleting non-existent device should not error (idempotent).
	_, err := service.UnregisterPushToken(ctx, &apiv1.UnregisterPushTokenRequest{
		DeviceId: "non-existent",
	})
	if err != nil {
		t.Fatalf("unregister non-existent should not error: %v", err)
	}
}

func TestRegisterPushToken_NilConfigStore(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.configStore = nil
	service := &authService{api: apiServer}

	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc_def]",
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for nil config store")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", status.Code(err))
	}
}

func TestUnregisterPushToken_DeviceIDTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	longID := strings.Repeat("x", 257)
	_, err := service.UnregisterPushToken(context.Background(), &apiv1.UnregisterPushTokenRequest{
		DeviceId: longID,
	})
	if err == nil {
		t.Fatal("expected error for device_id exceeding max length")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

func TestUnregisterPushToken_NilConfigStore(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.configStore = nil
	service := &authService{api: apiServer}

	_, err := service.UnregisterPushToken(context.Background(), &apiv1.UnregisterPushTokenRequest{
		DeviceId: "device-1",
	})
	if err == nil {
		t.Fatal("expected error for nil config store")
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("expected Unavailable, got %v", status.Code(err))
	}
}

func TestDeleteToken_CascadePushTokenCleanup(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := context.Background()

	// 1. Create two auth tokens.
	createResp1, err := service.CreateToken(ctx, &apiv1.CreateTokenRequest{
		Name: "device-1",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreateToken 1: %v", err)
	}
	createResp2, err := service.CreateToken(ctx, &apiv1.CreateTokenRequest{
		Name: "device-2",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreateToken 2: %v", err)
	}

	// 2. Register push tokens linked to each auth token via authenticated context.
	authCtx1 := apiServer.ContextWithToken(ctx, storedToken{
		ID:    createResp1.GetInfo().GetId(),
		Token: createResp1.GetToken(),
		Role:  "admin",
	})
	_, err = service.RegisterPushToken(authCtx1, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[cascade-tok-1]",
		DeviceId: "device-cascade-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("RegisterPushToken 1: %v", err)
	}

	authCtx2 := apiServer.ContextWithToken(ctx, storedToken{
		ID:    createResp2.GetInfo().GetId(),
		Token: createResp2.GetToken(),
		Role:  "admin",
	})
	_, err = service.RegisterPushToken(authCtx2, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[cascade-tok-2]",
		DeviceId: "device-cascade-2",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("RegisterPushToken 2: %v", err)
	}

	// Verify both push tokens exist.
	tokens, err := apiServer.configStore.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("ListPushTokens before delete: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 push tokens before cascade, got %d", len(tokens))
	}

	// 3. Delete auth token 1 — should only cascade-remove push token for device 1.
	_, err = service.DeleteToken(ctx, &apiv1.DeleteTokenRequest{
		Token: createResp1.GetToken(),
	})
	if err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}

	// 4. Verify only device-2's push token remains.
	tokens, err = apiServer.configStore.ListPushTokens(ctx)
	if err != nil {
		t.Fatalf("ListPushTokens after delete: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 push token after scoped cascade, got %d", len(tokens))
	}
	if tokens[0].DeviceID != "device-cascade-2" {
		t.Errorf("remaining device = %q, want device-cascade-2", tokens[0].DeviceID)
	}
}

// M2/R5: verify token max length validation.
func TestRegisterPushToken_TokenTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	longToken := "ExponentPushToken[" + strings.Repeat("x", 300) + "]"
	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    longToken,
		DeviceId: "device-1",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for token exceeding max length")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

// M2/R5: verify device_id max length validation.
func TestRegisterPushToken_DeviceIDTooLong(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}

	longDeviceID := strings.Repeat("d", 300)
	_, err := service.RegisterPushToken(context.Background(), &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[test_abc12345]",
		DeviceId: longDeviceID,
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err == nil {
		t.Fatal("expected error for device_id exceeding max length")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", status.Code(err))
	}
}

// L3/R5: verify ownership check — device registered by token A cannot be
// overwritten by token B.
func TestRegisterPushToken_OwnershipDenied(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	service := &authService{api: apiServer}
	ctx := context.Background()

	// Create two auth tokens.
	resp1, err := service.CreateToken(ctx, &apiv1.CreateTokenRequest{
		Name: "owner-device",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreateToken 1: %v", err)
	}
	resp2, err := service.CreateToken(ctx, &apiv1.CreateTokenRequest{
		Name: "intruder-device",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreateToken 2: %v", err)
	}

	// Register push token as token A.
	authCtx1 := apiServer.ContextWithToken(ctx, storedToken{
		ID:    resp1.GetInfo().GetId(),
		Token: resp1.GetToken(),
		Role:  "admin",
	})
	_, err = service.RegisterPushToken(authCtx1, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[owned_dev_1]",
		DeviceId: "shared-device",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR,
		},
	})
	if err != nil {
		t.Fatalf("RegisterPushToken as owner: %v", err)
	}

	// Attempt to overwrite as token B — should be denied.
	authCtx2 := apiServer.ContextWithToken(ctx, storedToken{
		ID:    resp2.GetInfo().GetId(),
		Token: resp2.GetToken(),
		Role:  "admin",
	})
	_, err = service.RegisterPushToken(authCtx2, &apiv1.RegisterPushTokenRequest{
		Token:    "ExponentPushToken[hijacked_tok]",
		DeviceId: "shared-device",
		EnabledEvents: []apiv1.NotificationEventType{
			apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_TASK_COMPLETED,
		},
	})
	if err == nil {
		t.Fatal("expected PermissionDenied for ownership violation")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
	}

	// Verify original token is unchanged.
	tok, err := apiServer.configStore.GetPushToken(ctx, "shared-device")
	if err != nil {
		t.Fatalf("GetPushToken: %v", err)
	}
	if tok.Token != "ExponentPushToken[owned_dev_1]" {
		t.Errorf("token = %q, want ExponentPushToken[owned_dev_1] (should not be overwritten)", tok.Token)
	}
}

func authCtxWithToken(t *testing.T, apiServer *APIServer, service *authService) context.Context {
	t.Helper()

	resp, err := service.CreateToken(context.Background(), &apiv1.CreateTokenRequest{
		Name: "test-device",
		Role: "admin",
	})
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}

	return apiServer.ContextWithToken(context.Background(), storedToken{
		ID:    resp.GetInfo().GetId(),
		Token: resp.GetToken(),
		Role:  "admin",
	})
}
