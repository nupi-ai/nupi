package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PushToken represents a registered push notification token for a mobile device.
type PushToken struct {
	DeviceID      string   `json:"device_id"`
	Token         string   `json:"token"`
	EnabledEvents []string `json:"enabled_events"`
	AuthTokenID   string   `json:"auth_token_id"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// validatePushTokenInput validates and normalises the common inputs for push
// token save operations. Returns the trimmed deviceID, token, and marshalled
// enabledEvents JSON, or an error if validation fails.
const maxPushTokenFieldLen = 256

func validatePushTokenInput(deviceID, token string, enabledEvents []string) (string, string, []byte, error) {
	deviceID = strings.TrimSpace(deviceID)
	token = strings.TrimSpace(token)
	if deviceID == "" {
		return "", "", nil, fmt.Errorf("config: save push token: device_id required")
	}
	if len(deviceID) > maxPushTokenFieldLen {
		return "", "", nil, fmt.Errorf("config: save push token: device_id exceeds maximum length (%d)", maxPushTokenFieldLen)
	}
	if token == "" {
		return "", "", nil, fmt.Errorf("config: save push token: token required")
	}
	if len(token) > maxPushTokenFieldLen {
		return "", "", nil, fmt.Errorf("config: save push token: token exceeds maximum length (%d)", maxPushTokenFieldLen)
	}
	if len(enabledEvents) == 0 {
		return "", "", nil, fmt.Errorf("config: save push token: at least one enabled event required")
	}
	eventsJSON, err := json.Marshal(enabledEvents)
	if err != nil {
		return "", "", nil, fmt.Errorf("config: save push token: marshal enabled events: %w", err)
	}
	return deviceID, token, eventsJSON, nil
}

// Deprecated: SavePushToken upserts without an ownership check. Use
// SavePushTokenOwned for ownership-safe operations from RPC handlers.
// This method is retained for tests and internal admin operations only.
func (s *Store) SavePushToken(ctx context.Context, deviceID, token string, enabledEvents []string, authTokenID string) error {
	if err := s.ensureWritable("save push token"); err != nil {
		return err
	}
	deviceID, token, eventsJSON, err := validatePushTokenInput(deviceID, token, enabledEvents)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO push_tokens (device_id, token, enabled_events, auth_token_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			token = excluded.token,
			enabled_events = excluded.enabled_events,
			auth_token_id = excluded.auth_token_id,
			updated_at = CURRENT_TIMESTAMP
	`, deviceID, token, string(eventsJSON), authTokenID)
	if err != nil {
		return fmt.Errorf("config: save push token: %w", err)
	}
	return nil
}

// SavePushTokenOwned atomically upserts a push token with an ownership check.
// The update only proceeds if no existing token is registered for this device,
// or if the existing token was registered by the same authTokenID.
// Returns true if the insert or update was applied, false if ownership denied.
func (s *Store) SavePushTokenOwned(ctx context.Context, deviceID, token string, enabledEvents []string, authTokenID string) (bool, error) {
	if err := s.ensureWritable("save push token"); err != nil {
		return false, err
	}
	deviceID, token, eventsJSON, err := validatePushTokenInput(deviceID, token, enabledEvents)
	if err != nil {
		return false, err
	}

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO push_tokens (device_id, token, enabled_events, auth_token_id)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(device_id) DO UPDATE SET
			token = excluded.token,
			enabled_events = excluded.enabled_events,
			auth_token_id = excluded.auth_token_id,
			updated_at = CURRENT_TIMESTAMP
		WHERE push_tokens.auth_token_id = '' OR push_tokens.auth_token_id = ?
	`, deviceID, token, string(eventsJSON), authTokenID, authTokenID)
	if err != nil {
		return false, fmt.Errorf("config: save push token: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("config: save push token: check rows: %w", err)
	}
	return rows > 0, nil
}

// GetPushToken returns a push token by device ID, or nil if not found.
func (s *Store) GetPushToken(ctx context.Context, deviceID string) (*PushToken, error) {
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return nil, fmt.Errorf("config: get push token: device_id required")
	}

	row := s.db.QueryRowContext(ctx, `
		SELECT device_id, token, enabled_events, auth_token_id, created_at, updated_at
		FROM push_tokens WHERE device_id = ?
	`, deviceID)

	pt, err := scanPushToken(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("config: get push token: %w", err)
	}
	return &pt, nil
}

// DeletePushTokenOwned atomically deletes a push token only if the requesting
// auth token owns it (or the token has no owner). Returns true if deleted,
// false if the device was not found or ownership was denied. When false is
// returned, callers can check device existence separately to distinguish the
// two cases.
func (s *Store) DeletePushTokenOwned(ctx context.Context, deviceID, authTokenID string) (bool, error) {
	if err := s.ensureWritable("delete push token owned"); err != nil {
		return false, err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return false, fmt.Errorf("config: delete push token owned: device_id required")
	}

	result, err := s.db.ExecContext(ctx, `
		DELETE FROM push_tokens
		WHERE device_id = ? AND (auth_token_id = '' OR auth_token_id = ?)
	`, deviceID, authTokenID)
	if err != nil {
		return false, fmt.Errorf("config: delete push token owned: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("config: delete push token owned: check rows: %w", err)
	}
	return rows > 0, nil
}

// DeletePushToken removes a push token by device ID.
// Returns nil if the device was not found (idempotent delete).
func (s *Store) DeletePushToken(ctx context.Context, deviceID string) error {
	if err := s.ensureWritable("delete push token"); err != nil {
		return err
	}
	deviceID = strings.TrimSpace(deviceID)
	if deviceID == "" {
		return fmt.Errorf("config: delete push token: device_id required")
	}

	_, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE device_id = ?`, deviceID)
	if err != nil {
		return fmt.Errorf("config: delete push token: %w", err)
	}
	return nil
}

// ListPushTokens returns all registered push tokens.
func (s *Store) ListPushTokens(ctx context.Context) ([]PushToken, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT device_id, token, enabled_events, auth_token_id, created_at, updated_at
		FROM push_tokens
		ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("config: list push tokens: %w", err)
	}
	defer rows.Close()

	return scanPushTokenRows(rows)
}

// ListPushTokensForEvent returns push tokens that have the given event type enabled.
func (s *Store) ListPushTokensForEvent(ctx context.Context, eventType string) ([]PushToken, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return nil, fmt.Errorf("config: list push tokens for event: event type required")
	}

	// SQLite JSON: use json_each to check if the event is in the enabled_events array.
	rows, err := s.db.QueryContext(ctx, `
		SELECT pt.device_id, pt.token, pt.enabled_events, pt.auth_token_id, pt.created_at, pt.updated_at
		FROM push_tokens pt
		WHERE EXISTS (
			SELECT 1 FROM json_each(pt.enabled_events) je WHERE je.value = ?
		)
		ORDER BY pt.created_at
	`, eventType)
	if err != nil {
		return nil, fmt.Errorf("config: list push tokens for event %q: %w", eventType, err)
	}
	defer rows.Close()

	return scanPushTokenRows(rows)
}

// DeletePushTokensByAuthToken removes push tokens linked to a specific auth token.
func (s *Store) DeletePushTokensByAuthToken(ctx context.Context, authTokenID string) error {
	if err := s.ensureWritable("delete push tokens by auth token"); err != nil {
		return err
	}
	authTokenID = strings.TrimSpace(authTokenID)
	if authTokenID == "" {
		return fmt.Errorf("config: delete push tokens by auth token: auth_token_id required")
	}

	_, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens WHERE auth_token_id = ?`, authTokenID)
	if err != nil {
		return fmt.Errorf("config: delete push tokens by auth token: %w", err)
	}
	return nil
}

// DeleteAllPushTokens removes all push tokens. Used for cleanup during
// testing or when all pairings are revoked.
func (s *Store) DeleteAllPushTokens(ctx context.Context) error {
	if err := s.ensureWritable("delete all push tokens"); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `DELETE FROM push_tokens`)
	if err != nil {
		return fmt.Errorf("config: delete all push tokens: %w", err)
	}
	return nil
}

func scanPushTokenRows(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]PushToken, error) {
	var result []PushToken
	for rows.Next() {
		pt, err := scanPushToken(rows)
		if err != nil {
			return nil, fmt.Errorf("config: scan push token: %w", err)
		}
		result = append(result, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("config: iterate push tokens: %w", err)
	}
	if result == nil {
		result = []PushToken{}
	}
	return result, nil
}

func scanPushToken(scanner interface {
	Scan(dest ...any) error
}) (PushToken, error) {
	var (
		token      PushToken
		eventsJSON string
	)
	if err := scanner.Scan(
		&token.DeviceID,
		&token.Token,
		&eventsJSON,
		&token.AuthTokenID,
		&token.CreatedAt,
		&token.UpdatedAt,
	); err != nil {
		return PushToken{}, err
	}
	if err := json.Unmarshal([]byte(eventsJSON), &token.EnabledEvents); err != nil {
		return PushToken{}, fmt.Errorf("config: unmarshal push token events: %w", err)
	}
	return token, nil
}
