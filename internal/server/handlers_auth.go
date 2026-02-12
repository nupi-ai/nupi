package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

var allowedRoles = map[string]struct{}{
	string(roleAdmin):    {},
	string(roleReadOnly): {},
}

func sanitizeTokens(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}

	unique := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		trimmed := strings.TrimSpace(token)
		if trimmed == "" {
			continue
		}
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}

func sanitizeStoredTokens(tokens []storedToken) []storedToken {
	if len(tokens) == 0 {
		return nil
	}

	seenTokens := make(map[string]struct{}, len(tokens))
	seenIDs := make(map[string]struct{}, len(tokens))
	result := make([]storedToken, 0, len(tokens))

	for _, token := range tokens {
		token.Token = strings.TrimSpace(token.Token)
		if token.Token == "" {
			continue
		}
		if _, exists := seenTokens[token.Token]; exists {
			continue
		}
		token.Role = normalizeRole(token.Role)
		token.Name = strings.TrimSpace(token.Name)
		if token.CreatedAt.IsZero() {
			token.CreatedAt = time.Now().UTC()
		}
		id := strings.TrimSpace(token.ID)
		if id == "" {
			id = defaultTokenID(token.Token)
		}
		for {
			if _, exists := seenIDs[id]; !exists {
				break
			}
			id = defaultTokenID(token.Token) + "-" + generateRandomID()
		}
		token.ID = id
		seenIDs[id] = struct{}{}
		seenTokens[token.Token] = struct{}{}
		result = append(result, token)
	}

	return result
}

func newStoredToken(token, name, role string) storedToken {
	entry := storedToken{
		Token:     strings.TrimSpace(token),
		Name:      strings.TrimSpace(name),
		Role:      normalizeRole(role),
		CreatedAt: time.Now().UTC(),
	}
	processed := sanitizeStoredTokens([]storedToken{entry})
	if len(processed) == 0 {
		return storedToken{}
	}
	return processed[0]
}

func normalizeRole(role string) string {
	trimmed := strings.TrimSpace(strings.ToLower(role))
	if trimmed == "" {
		return string(roleAdmin)
	}
	if _, ok := allowedRoles[trimmed]; ok {
		return trimmed
	}
	return string(roleAdmin)
}

func defaultTokenID(token string) string {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) >= 12 {
		return trimmed[:12]
	}
	if len(trimmed) >= 4 {
		return trimmed
	}
	return generateRandomID()
}

func generateRandomID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func generateAPIToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate api token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

type tokenResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	Role        string `json:"role"`
	MaskedToken string `json:"masked_token"`
	CreatedAt   string `json:"created_at"`
}

func maskToken(token string) string {
	if len(token) <= 8 {
		return token
	}
	return token[:4] + strings.Repeat("*", len(token)-8) + token[len(token)-4:]
}

func (s *APIServer) handleAuthTokens(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAuthTokensGet(w, r)
	case http.MethodPost:
		s.handleAuthTokensPost(w, r)
	case http.MethodDelete:
		s.handleAuthTokensDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAuthTokensGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	tokens, err := s.loadAuthTokens(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Tokens []tokenResponse `json:"tokens"`
	}{
		Tokens: make([]tokenResponse, 0, len(tokens)),
	}

	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i].CreatedAt.Before(tokens[j].CreatedAt)
	})

	for _, token := range tokens {
		entry := tokenResponse{
			ID:          token.ID,
			Name:        token.Name,
			Role:        token.Role,
			MaskedToken: maskToken(token.Token),
			CreatedAt:   token.CreatedAt.Format(time.RFC3339),
		}
		resp.Tokens = append(resp.Tokens, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthTokensPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	ctx := r.Context()
	tokens, err := s.loadAuthTokens(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var payload struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil && err != io.EOF {
			http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
			return
		}
	}

	token, err := generateAPIToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	entry := newStoredToken(token, payload.Name, payload.Role)
	if entry.Token == "" {
		http.Error(w, "failed to create token entry", http.StatusInternalServerError)
		return
	}

	tokens = append(tokens, entry)
	if err := s.storeAuthTokens(ctx, tokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist token: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(tokens, s.AuthRequired())

	resp := struct {
		Token string `json:"token"`
		ID    string `json:"id"`
		Name  string `json:"name,omitempty"`
		Role  string `json:"role"`
	}{
		Token: token,
		ID:    entry.ID,
		Name:  entry.Name,
		Role:  entry.Role,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthTokensDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	var payload struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	target := strings.TrimSpace(payload.Token)
	id := strings.TrimSpace(payload.ID)
	if target == "" && id == "" {
		http.Error(w, "token or id is required", http.StatusBadRequest)
		return
	}

	tokens, err := s.loadAuthTokens(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newTokens := make([]storedToken, 0, len(tokens))
	removed := false
	for _, tok := range tokens {
		if (target != "" && tok.Token == target) || (id != "" && tok.ID == id) {
			removed = true
			continue
		}
		newTokens = append(newTokens, tok)
	}

	if !removed {
		http.Error(w, "token not found", http.StatusNotFound)
		return
	}

	if err := s.storeAuthTokens(r.Context(), newTokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist tokens: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(newTokens, s.AuthRequired())
	w.WriteHeader(http.StatusNoContent)
}

func (s *APIServer) handleAuthPairings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet:
		s.handleAuthPairingsGet(w, r)
	case http.MethodPost:
		s.handleAuthPairingsPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAuthPairingsGet(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	pairings, err := s.loadPairings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Pairings []pairingEntry `json:"pairings"`
	}{
		Pairings: sanitizePairings(pairings, time.Now().UTC()),
	}

	if err := s.storePairings(r.Context(), resp.Pairings); err != nil {
		log.Printf("[AuthPairings] failed to persist pairings cleanup: %v", err)
	}

	sort.Slice(resp.Pairings, func(i, j int) bool {
		return resp.Pairings[i].ExpiresAt.Before(resp.Pairings[j].ExpiresAt)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthPairingsPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireRole(w, r, roleAdmin); !ok {
		return
	}

	pairings, err := s.loadPairings(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var payload struct {
		Name      string `json:"name"`
		Role      string `json:"role"`
		ExpiresIn int    `json:"expires_in_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	duration := time.Duration(payload.ExpiresIn) * time.Second
	if duration <= 0 || duration > 30*time.Minute {
		duration = 5 * time.Minute
	}

	code, err := generatePairingCode()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate pairing code: %v", err), http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	entry := pairingEntry{
		Code:      code,
		Name:      strings.TrimSpace(payload.Name),
		Role:      normalizeRole(payload.Role),
		CreatedAt: now,
		ExpiresAt: now.Add(duration),
	}

	pairings = append(pairings, entry)
	pairings = sanitizePairings(pairings, now)
	if err := s.storePairings(r.Context(), pairings); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist pairing: %v", err), http.StatusInternalServerError)
		return
	}

	resp := struct {
		Code      string `json:"pair_code"`
		Name      string `json:"name,omitempty"`
		Role      string `json:"role"`
		ExpiresAt string `json:"expires_at"`
	}{
		Code:      entry.Code,
		Name:      entry.Name,
		Role:      entry.Role,
		ExpiresAt: entry.ExpiresAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) handleAuthPair(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodPost:
		s.handleAuthPairClaim(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *APIServer) handleAuthPairClaim(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Code string `json:"code"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON payload: %v", err), http.StatusBadRequest)
		return
	}

	code := strings.ToUpper(strings.TrimSpace(payload.Code))
	if code == "" {
		http.Error(w, "code is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	pairings, err := s.loadPairings(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	index := -1
	var entry pairingEntry
	for i, pairing := range pairings {
		if strings.EqualFold(pairing.Code, code) {
			entry = pairing
			index = i
			break
		}
	}

	if index == -1 {
		http.Error(w, "pairing code not found", http.StatusNotFound)
		return
	}

	pairings = append(pairings[:index], pairings[index+1:]...)
	pairings = sanitizePairings(pairings, now)
	if err := s.storePairings(ctx, pairings); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist pairings: %v", err), http.StatusInternalServerError)
		return
	}

	if now.After(entry.ExpiresAt) {
		http.Error(w, "pairing code expired", http.StatusGone)
		return
	}

	tokens, err := s.loadAuthTokens(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	newTokenValue, err := generateAPIToken()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to generate token: %v", err), http.StatusInternalServerError)
		return
	}

	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = entry.Name
	}

	newEntry := newStoredToken(newTokenValue, name, entry.Role)
	if newEntry.Token == "" {
		http.Error(w, "failed to create token entry", http.StatusInternalServerError)
		return
	}

	tokens = append(tokens, newEntry)
	if err := s.storeAuthTokens(ctx, tokens); err != nil {
		http.Error(w, fmt.Sprintf("failed to persist token: %v", err), http.StatusInternalServerError)
		return
	}

	s.setAuthTokens(tokens, s.AuthRequired())

	resp := struct {
		Token     string `json:"token"`
		Name      string `json:"name,omitempty"`
		Role      string `json:"role"`
		CreatedAt string `json:"created_at"`
	}{
		Token:     newTokenValue,
		Name:      newEntry.Name,
		Role:      newEntry.Role,
		CreatedAt: newEntry.CreatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *APIServer) loadPairings(ctx context.Context) ([]pairingEntry, error) {
	if s.configStore == nil {
		return nil, nil
	}

	values, err := s.configStore.LoadSecuritySettings(ctx, "auth.pairings")
	if err != nil {
		return nil, err
	}

	raw, ok := values["auth.pairings"]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	var entries []pairingEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse auth.pairings: %w", err)
	}

	return sanitizePairings(entries, time.Now().UTC()), nil
}

func (s *APIServer) storePairings(ctx context.Context, entries []pairingEntry) error {
	if s.configStore == nil {
		return nil
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return s.configStore.SaveSecuritySettings(ctx, map[string]string{
		"auth.pairings": string(payload),
	})
}

func sanitizePairings(entries []pairingEntry, now time.Time) []pairingEntry {
	if len(entries) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	result := make([]pairingEntry, 0, len(entries))
	for _, entry := range entries {
		entry.Code = strings.ToUpper(strings.TrimSpace(entry.Code))
		if entry.Code == "" {
			continue
		}
		if _, exists := seen[entry.Code]; exists {
			continue
		}
		if entry.Role == "" {
			entry.Role = string(roleAdmin)
		} else {
			entry.Role = normalizeRole(entry.Role)
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = now
		}
		if entry.ExpiresAt.IsZero() {
			entry.ExpiresAt = entry.CreatedAt.Add(5 * time.Minute)
		}
		if now.After(entry.ExpiresAt) {
			continue
		}
		seen[entry.Code] = struct{}{}
		result = append(result, entry)
	}
	return result
}

func generatePairingCode() (string, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	code := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	if len(code) > 10 {
		code = code[:10]
	}
	return code, nil
}
