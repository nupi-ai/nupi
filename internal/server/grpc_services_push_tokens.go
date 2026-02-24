package server

import (
	"context"
	"regexp"
	"strings"

	apiv1 "github.com/nupi-ai/nupi/internal/api/grpc/v1"
	"github.com/nupi-ai/nupi/internal/constants"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var expoPushTokenRegexp = regexp.MustCompile(`^ExponentPushToken\[[A-Za-z0-9._-]{10,}\]$`)

var deviceIDRegexp = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const (
	maxPushTokenLen = 256
	maxDeviceIDLen  = 256
)

func (a *authService) RegisterPushToken(ctx context.Context, req *apiv1.RegisterPushTokenRequest) (*apiv1.RegisterPushTokenResponse, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}

	token := strings.TrimSpace(req.GetToken())
	if token == "" {
		return nil, status.Error(codes.InvalidArgument, "token is required")
	}
	if len(token) > maxPushTokenLen {
		return nil, status.Error(codes.InvalidArgument, "token exceeds maximum length")
	}
	if !expoPushTokenRegexp.MatchString(token) {
		return nil, status.Error(codes.InvalidArgument, "token must be a valid Expo push token (ExponentPushToken[...])")
	}

	deviceID := strings.TrimSpace(req.GetDeviceId())
	if deviceID == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if len(deviceID) > maxDeviceIDLen {
		return nil, status.Error(codes.InvalidArgument, "device_id exceeds maximum length")
	}
	if !deviceIDRegexp.MatchString(deviceID) {
		return nil, status.Error(codes.InvalidArgument, "device_id contains invalid characters")
	}

	protoEvents := req.GetEnabledEvents()
	if len(protoEvents) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one enabled_event is required")
	}

	// Validate and deduplicate event types. M4 fix (Review 14): dedup is done
	// after string conversion to prevent theoretical collisions where two proto
	// enum values map to the same string.
	seenStrings := make(map[string]bool, len(protoEvents))
	events := make([]string, 0, len(protoEvents))
	for _, ev := range protoEvents {
		if !isKnownNotificationEventType(ev) {
			return nil, status.Errorf(codes.InvalidArgument, "unknown event type: %v", ev)
		}
		s := notificationEventTypeToString(ev)
		if seenStrings[s] {
			continue
		}
		seenStrings[s] = true
		events = append(events, s)
	}

	// Link push token to the auth token that registered it, so cascade
	// deletion on auth token revocation only removes this device's tokens.
	authTok, ok := tokenFromContext(ctx)
	if !ok || authTok.ID == "" {
		return nil, status.Error(codes.Unauthenticated, "valid auth token required to register push token")
	}
	authTokenID := authTok.ID

	saved, err := a.api.configStore.SavePushTokenOwned(ctx, deviceID, token, events, authTokenID)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to save push token")
	}
	if !saved {
		return nil, status.Error(codes.PermissionDenied, "device registered by another token")
	}

	return &apiv1.RegisterPushTokenResponse{}, nil
}

func (a *authService) UnregisterPushToken(ctx context.Context, req *apiv1.UnregisterPushTokenRequest) (*apiv1.UnregisterPushTokenResponse, error) {
	if a.api.configStore == nil {
		return nil, status.Error(codes.Unavailable, "configuration store unavailable")
	}

	deviceID := strings.TrimSpace(req.GetDeviceId())
	if deviceID == "" {
		return nil, status.Error(codes.InvalidArgument, "device_id is required")
	}
	if len(deviceID) > maxDeviceIDLen {
		return nil, status.Error(codes.InvalidArgument, "device_id exceeds maximum length")
	}
	if !deviceIDRegexp.MatchString(deviceID) {
		return nil, status.Error(codes.InvalidArgument, "device_id contains invalid characters")
	}

	// Atomically delete only if the requesting auth token owns the device
	// (or the device has no owner). Avoids TOCTOU race between ownership
	// check and deletion.
	authTok, ok := tokenFromContext(ctx)
	if !ok || authTok.ID == "" {
		return nil, status.Error(codes.Unauthenticated, "valid auth token required to unregister push token")
	}
	authTokenID := authTok.ID

	deleted, err := a.api.configStore.DeletePushTokenOwned(ctx, deviceID, authTokenID)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to delete push token")
	}
	if !deleted {
		// Distinguish "not found" (idempotent success) from "ownership denied".
		existing, err := a.api.configStore.GetPushToken(ctx, deviceID)
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to check push token ownership")
		}
		if existing != nil {
			return nil, status.Error(codes.PermissionDenied, "device registered by another token")
		}
		// Device not found — idempotent success, fall through.
	}

	return &apiv1.UnregisterPushTokenResponse{}, nil
}

// isKnownNotificationEventType returns true for valid, non-UNSPECIFIED event types.
func isKnownNotificationEventType(t apiv1.NotificationEventType) bool {
	switch t {
	case apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_TASK_COMPLETED,
		apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_INPUT_NEEDED,
		apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR:
		return true
	default:
		return false
	}
}

func notificationEventTypeToString(t apiv1.NotificationEventType) string {
	switch t {
	case apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_TASK_COMPLETED:
		return constants.NotificationEventTaskCompleted
	case apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_INPUT_NEEDED:
		return constants.NotificationEventInputNeeded
	case apiv1.NotificationEventType_NOTIFICATION_EVENT_TYPE_ERROR:
		return constants.NotificationEventError
	default:
		// Unreachable: isKnownNotificationEventType rejects unknown types before
		// this function is called. Kept as defensive fallback for future enum additions.
		s := t.String()
		const prefix = "NOTIFICATION_EVENT_TYPE_"
		if strings.HasPrefix(s, prefix) {
			return s[len(prefix):]
		}
		return s
	}
}
