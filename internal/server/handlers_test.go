package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/nupi-ai/nupi/internal/adapterrunner"
	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/audio/egress"
	"github.com/nupi-ai/nupi/internal/audio/ingress"
	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/contentpipeline"
	"github.com/nupi-ai/nupi/internal/eventbus"
	"github.com/nupi-ai/nupi/internal/modules"
	"github.com/nupi-ai/nupi/internal/observability"
	"github.com/nupi-ai/nupi/internal/pty"
	"github.com/nupi-ai/nupi/internal/session"
)

func TestRegisterSessionListenerIdempotent(t *testing.T) {
	apiServer, sessionManager := newTestAPIServer(t)

	if got := listenerCount(sessionManager); got != 1 {
		t.Fatalf("expected 1 listener after creation, got %d", got)
	}

	// Simulate APIServer.Start invoking listener registration again.
	apiServer.registerSessionListener()

	if got := listenerCount(sessionManager); got != 1 {
		t.Fatalf("expected listener registration to be idempotent, got %d", got)
	}
}

func listenerCount(m *session.Manager) int {
	val := reflect.ValueOf(m).Elem().FieldByName("listeners")
	return val.Len()
}

func withRole(s *APIServer, req *http.Request, role tokenRole) *http.Request {
	tokenValue := fmt.Sprintf("%s-test-token", role)
	entry := newStoredToken(tokenValue, string(role), string(role))

	s.authMu.Lock()
	if s.authTokens == nil {
		s.authTokens = make(map[string]storedToken)
	}
	s.authTokens[tokenValue] = entry
	s.authRequired = true
	s.authMu.Unlock()

	req.Header.Set("Authorization", "Bearer "+tokenValue)
	req.Header.Set("X-Nupi-Token", tokenValue)

	ctx := context.WithValue(req.Context(), authContextKey{}, entry)
	return req.WithContext(ctx)
}

func withAdmin(s *APIServer, req *http.Request) *http.Request {
	return withRole(s, req, roleAdmin)
}

func withReadOnly(s *APIServer, req *http.Request) *http.Request {
	return withRole(s, req, roleReadOnly)
}

type mockConversationStore struct {
	turns map[string][]eventbus.ConversationTurn
}

func (m *mockConversationStore) Context(sessionID string) []eventbus.ConversationTurn {
	if m == nil {
		return nil
	}
	turns := m.turns[sessionID]
	out := make([]eventbus.ConversationTurn, len(turns))
	copy(out, turns)
	return out
}

func (m *mockConversationStore) Slice(sessionID string, offset, limit int) (int, []eventbus.ConversationTurn) {
	if m == nil {
		return 0, nil
	}
	raw := m.turns[sessionID]
	total := len(raw)
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	window := raw[offset:end]
	out := make([]eventbus.ConversationTurn, len(window))
	copy(out, window)
	return total, out
}

func writeSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName: "localhost",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}

	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert file: %v", err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	return certPath, keyPath
}

func TestHandleRecordingsListOptions(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/recordings", nil)
	rec := httptest.NewRecorder()

	apiServer.handleRecordingsList(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestHandleRecordingFileOptions(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/recordings/test-id", nil)
	rec := httptest.NewRecorder()

	apiServer.handleRecordingFile(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestHandleTransportConfigGet(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := httptest.NewRequest(http.MethodGet, "/config/transport", nil)
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v (body=%q)", err, rec.Body.String())
	}

	if payload["binding"] != "loopback" {
		t.Fatalf("expected binding to default to loopback, got %v", payload["binding"])
	}
	if gp, ok := payload["grpc_port"].(float64); !ok || gp != 0 {
		t.Fatalf("expected grpc_port to default to 0, got %v", payload["grpc_port"])
	}
	if payload["grpc_binding"] != "loopback" {
		t.Fatalf("expected grpc_binding to default to loopback, got %v", payload["grpc_binding"])
	}
	if authRequired, ok := payload["auth_required"].(bool); !ok || authRequired {
		t.Fatalf("expected auth_required to be false by default, got %v", payload["auth_required"])
	}
}

func TestHandleTransportConfigUpdate(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	tmpDir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, tmpDir)

	body := fmt.Sprintf(`{"binding":"lan","port":8081,"tls_cert_path":%q,"tls_key_path":%q}`, certPath, keyPath)
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPut, "/config/transport", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	token := rec.Header().Get("X-Nupi-API-Token")
	if token == "" {
		t.Fatalf("expected generated API token in response header")
	}

	var updateResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &updateResp); err != nil {
		t.Fatalf("invalid JSON response for update: %v (body=%q)", err, rec.Body.String())
	}
	if updateResp["binding"] != "lan" {
		t.Fatalf("expected binding lan in update response, got %v", updateResp["binding"])
	}
	if updateResp["auth_token"] != token {
		t.Fatalf("expected auth_token in body to match header")
	}
	if updateResp["grpc_binding"] != "lan" {
		t.Fatalf("expected grpc_binding lan in update response, got %v", updateResp["grpc_binding"])
	}
	if gp, ok := updateResp["grpc_port"].(float64); !ok || gp != 0 {
		t.Fatalf("expected grpc_port 0 in update response, got %v", updateResp["grpc_port"])
	}

	reqGet := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/config/transport", nil))
	recGet := httptest.NewRecorder()
	apiServer.handleTransportConfig(recGet, reqGet)

	var resp transportResponse
	if err := json.Unmarshal(recGet.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v (body=%q)", err, recGet.Body.String())
	}

	if resp.Binding != "lan" {
		t.Fatalf("expected binding lan, got %s", resp.Binding)
	}
	if resp.Port != 8081 {
		t.Fatalf("expected port 8081, got %d", resp.Port)
	}
	if resp.GRPCBinding != "lan" {
		t.Fatalf("expected grpc binding lan, got %s", resp.GRPCBinding)
	}
	if resp.GRPCPort != 0 {
		t.Fatalf("expected grpc port 0, got %d", resp.GRPCPort)
	}
	if !resp.AuthRequired {
		t.Fatalf("expected auth_required to be true for LAN binding")
	}
}

func TestHandleTransportConfigUpdateRequiresTLS(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPut, "/config/transport", bytes.NewBufferString(`{"binding":"lan","port":8081}`)))
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d when TLS is missing, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestHandleTransportConfigUpdateInvalidBinding(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPut, "/config/transport", bytes.NewBufferString(`{"binding":"unknown","port":8081}`)))
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d for invalid binding, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestHandleTransportConfigUpdatePartialTLS(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	tmpDir := t.TempDir()
	certPath, _ := writeSelfSignedCert(t, tmpDir)

	payload := fmt.Sprintf(`{"binding":"lan","tls_cert_path":%q}`, certPath)
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPut, "/config/transport", bytes.NewBufferString(payload)))
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d when TLS configuration is partial, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestHandleTransportConfigUpdateGRPCBindingRequiresTLS(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	tmpDir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, tmpDir)

	body := fmt.Sprintf(`{"binding":"loopback","grpc_binding":"lan","tls_cert_path":%q,"tls_key_path":%q}`, certPath, keyPath)
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPut, "/config/transport", bytes.NewBufferString(body)))
	rec := httptest.NewRecorder()

	apiServer.handleTransportConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	token := rec.Header().Get("X-Nupi-API-Token")
	if token == "" {
		t.Fatalf("expected generated API token in response header")
	}

	var updateResp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &updateResp); err != nil {
		t.Fatalf("invalid JSON response for update: %v", err)
	}
	if updateResp["grpc_binding"] != "lan" {
		t.Fatalf("expected grpc_binding lan in update response, got %v", updateResp["grpc_binding"])
	}

	reqGet := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/config/transport", nil))
	recGet := httptest.NewRecorder()
	apiServer.handleTransportConfig(recGet, reqGet)

	var resp transportResponse
	if err := json.Unmarshal(recGet.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if resp.Binding != "loopback" {
		t.Fatalf("expected binding loopback, got %s", resp.Binding)
	}
	if resp.GRPCBinding != "lan" {
		t.Fatalf("expected grpc binding lan, got %s", resp.GRPCBinding)
	}
	if !resp.AuthRequired {
		t.Fatalf("expected auth_required to be true when gRPC binding is lan")
	}
}

func TestHandleConfigMigrate(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore

	if _, err := store.DB().ExecContext(context.Background(), `
		DELETE FROM adapter_bindings WHERE instance_name = ? AND profile_name = ? AND slot = 'tts.primary'
	`, store.InstanceName(), store.ProfileName()); err != nil {
		t.Fatalf("delete slot: %v", err)
	}

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/config/migrate", nil))
	rec := httptest.NewRecorder()

	apiServer.handleConfigMigrate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d (body=%s)", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload configMigrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	found := false
	for _, slot := range payload.UpdatedSlots {
		if slot == "tts.primary" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected tts.primary in updated slots, got %v", payload.UpdatedSlots)
	}

	bindings, err := store.ListAdapterBindings(context.Background())
	if err != nil {
		t.Fatalf("list bindings: %v", err)
	}

	var restored bool
	for _, binding := range bindings {
		if binding.Slot != "tts.primary" {
			continue
		}
		restored = true
		if binding.Status != configstore.BindingStatusRequired {
			t.Fatalf("expected restored slot to be required, got %s", binding.Status)
		}
		if binding.AdapterID != nil {
			t.Fatalf("expected adapter to remain unset, got %v", binding.AdapterID)
		}
	}
	if !restored {
		t.Fatalf("expected tts.primary slot to exist after migration")
	}
}

func TestHandleSessionAttachAndInput(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "cat"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := apiServer.sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer apiServer.sessionManager.KillSession(sess.ID)
	time.Sleep(100 * time.Millisecond)

	attachReq := withReadOnly(apiServer, httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/attach", bytes.NewBufferString(`{"include_history":false}`)))
	attachRec := httptest.NewRecorder()
	apiServer.handleSessionAttach(attachRec, attachReq)

	if attachRec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, attachRec.Code)
	}

	var attachPayload map[string]any
	if err := json.Unmarshal(attachRec.Body.Bytes(), &attachPayload); err != nil {
		t.Fatalf("invalid attach response: %v", err)
	}
	if _, ok := attachPayload["stream_url"].(string); !ok {
		t.Fatalf("stream_url missing or invalid")
	}

	inputReq := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/input", bytes.NewBufferString(`{"input":"hello\\n"}`)))
	inputRec := httptest.NewRecorder()
	apiServer.handleSessionInput(inputRec, inputReq)
	if inputRec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, inputRec.Code)
	}

	// Send EOF to terminate cat
	inputEOFReq := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/input", bytes.NewBufferString(`{"eof":true}`)))
	inputEOFRec := httptest.NewRecorder()
	apiServer.handleSessionInput(inputEOFRec, inputEOFReq)
	if inputEOFRec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, inputEOFRec.Code)
	}

	detachReq := withReadOnly(apiServer, httptest.NewRequest(http.MethodPost, "/sessions/"+sess.ID+"/detach", nil))
	detachRec := httptest.NewRecorder()
	apiServer.handleSessionDetach(detachRec, detachReq)
	if detachRec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, detachRec.Code)
	}
}

func TestHandleSessionConversationOptions(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := httptest.NewRequest(http.MethodOptions, "/sessions/abc/conversation", nil)
	rec := httptest.NewRecorder()

	apiServer.handleSessionConversation(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, rec.Code)
	}
}

func TestHandleSessionConversation(t *testing.T) {
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sessionManager.KillSession(sess.ID)
	time.Sleep(100 * time.Millisecond)

	now := time.Now().UTC()
	store := &mockConversationStore{
		turns: map[string][]eventbus.ConversationTurn{
			sess.ID: {
				{Origin: eventbus.OriginUser, Text: "hello", At: now},
				{Origin: eventbus.OriginAI, Text: "hi there", At: now.Add(10 * time.Millisecond), Meta: map[string]string{"source": "test"}},
			},
		},
	}
	apiServer.SetConversationStore(store)

	req := withReadOnly(apiServer, httptest.NewRequest(http.MethodGet, "/sessions/"+sess.ID+"/conversation", nil))
	rec := httptest.NewRecorder()

	apiServer.handleSessionConversation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d (body=%s)", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		SessionID  string `json:"session_id"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Total      int    `json:"total"`
		HasMore    bool   `json:"has_more"`
		NextOffset *int   `json:"next_offset"`
		Turns      []struct {
			Origin string            `json:"origin"`
			Text   string            `json:"text"`
			Meta   map[string]string `json:"meta"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if payload.SessionID != sess.ID {
		t.Fatalf("unexpected session id: %s", payload.SessionID)
	}
	if payload.Offset != 0 || payload.Total != 2 {
		t.Fatalf("unexpected pagination metadata: offset=%d total=%d", payload.Offset, payload.Total)
	}
	if payload.Limit != 2 {
		t.Fatalf("expected limit=2, got %d", payload.Limit)
	}
	if payload.HasMore {
		t.Fatalf("expected has_more=false")
	}
	if payload.NextOffset != nil {
		t.Fatalf("expected next_offset to be nil, got %v", *payload.NextOffset)
	}
	if len(payload.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(payload.Turns))
	}
	if payload.Turns[0].Origin != string(eventbus.OriginUser) || payload.Turns[0].Text != "hello" {
		t.Fatalf("unexpected first turn: %+v", payload.Turns[0])
	}
	if payload.Turns[1].Origin != string(eventbus.OriginAI) || payload.Turns[1].Text != "hi there" {
		t.Fatalf("unexpected second turn: %+v", payload.Turns[1])
	}
	if payload.Turns[1].Meta["source"] != "test" {
		t.Fatalf("expected metadata to be preserved, got %+v", payload.Turns[1].Meta)
	}
}

func TestHandleSessionConversationPagination(t *testing.T) {
	apiServer, sessionManager := newTestAPIServer(t)

	opts := pty.StartOptions{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 1"},
		Rows:    24,
		Cols:    80,
	}

	sess, err := sessionManager.CreateSession(opts, false)
	if err != nil {
		t.Fatalf("failed to create session: %v", err)
	}
	defer sessionManager.KillSession(sess.ID)

	now := time.Now().UTC()
	store := &mockConversationStore{
		turns: map[string][]eventbus.ConversationTurn{
			sess.ID: {
				{Origin: eventbus.OriginUser, Text: "0", At: now},
				{Origin: eventbus.OriginAI, Text: "1", At: now.Add(10 * time.Millisecond)},
				{Origin: eventbus.OriginUser, Text: "2", At: now.Add(20 * time.Millisecond)},
				{Origin: eventbus.OriginAI, Text: "3", At: now.Add(30 * time.Millisecond)},
			},
		},
	}
	apiServer.SetConversationStore(store)

	req := withReadOnly(apiServer, httptest.NewRequest(http.MethodGet, "/sessions/"+sess.ID+"/conversation?offset=1&limit=2", nil))
	rec := httptest.NewRecorder()

	apiServer.handleSessionConversation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d (body=%s)", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload struct {
		SessionID  string `json:"session_id"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		Total      int    `json:"total"`
		HasMore    bool   `json:"has_more"`
		NextOffset *int   `json:"next_offset"`
		Turns      []struct {
			Text string `json:"text"`
		} `json:"turns"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if payload.Offset != 1 || payload.Limit != 2 || payload.Total != 4 {
		t.Fatalf("unexpected pagination metadata: %+v", payload)
	}
	if !payload.HasMore {
		t.Fatalf("expected has_more=true")
	}
	if payload.NextOffset == nil || *payload.NextOffset != 3 {
		t.Fatalf("expected next_offset=3, got %v", payload.NextOffset)
	}
	if len(payload.Turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(payload.Turns))
	}
	if payload.Turns[0].Text != "1" || payload.Turns[1].Text != "2" {
		t.Fatalf("unexpected turns: %+v", payload.Turns)
	}
}

func TestHandleAuthTokensCRUD(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	reqList := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/auth/tokens", nil))
	recList := httptest.NewRecorder()
	apiServer.handleAuthTokens(recList, reqList)

	if recList.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recList.Code)
	}

	var listPayload map[string][]tokenResponse
	if err := json.Unmarshal(recList.Body.Bytes(), &listPayload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(listPayload["tokens"]) != 0 {
		t.Fatalf("expected no tokens initially")
	}

	recCreate := httptest.NewRecorder()
	reqCreate := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/auth/tokens", nil))
	apiServer.handleAuthTokens(recCreate, reqCreate)

	if recCreate.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recCreate.Code)
	}

	var createPayload struct {
		Token string `json:"token"`
		ID    string `json:"id"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if err := json.Unmarshal(recCreate.Body.Bytes(), &createPayload); err != nil {
		t.Fatalf("invalid create response: %v", err)
	}
	newToken := createPayload.Token
	if newToken == "" {
		t.Fatalf("expected token in response")
	}
	if createPayload.ID == "" {
		t.Fatalf("expected id in response")
	}

	reqList2 := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/auth/tokens", nil))
	recList2 := httptest.NewRecorder()
	apiServer.handleAuthTokens(recList2, reqList2)

	if recList2.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recList2.Code)
	}

	var listPayload2 map[string][]tokenResponse
	if err := json.Unmarshal(recList2.Body.Bytes(), &listPayload2); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(listPayload2["tokens"]) != 1 {
		t.Fatalf("expected one token in list")
	}
	if got := listPayload2["tokens"][0].Role; got != createPayload.Role {
		t.Fatalf("expected role %s, got %s", createPayload.Role, got)
	}
	if listPayload2["tokens"][0].MaskedToken == "" {
		t.Fatalf("expected masked token in listing")
	}

	recDelete := httptest.NewRecorder()
	deleteBody := bytes.NewBufferString(fmt.Sprintf(`{"id":%q}`, createPayload.ID))
	reqDelete := withAdmin(apiServer, httptest.NewRequest(http.MethodDelete, "/auth/tokens", deleteBody))
	apiServer.handleAuthTokens(recDelete, reqDelete)

	if recDelete.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, recDelete.Code)
	}

	reqList3 := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/auth/tokens", nil))
	recList3 := httptest.NewRecorder()
	apiServer.handleAuthTokens(recList3, reqList3)

	if recList3.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, recList3.Code)
	}

	var listPayload3 map[string][]tokenResponse
	if err := json.Unmarshal(recList3.Body.Bytes(), &listPayload3); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(listPayload3["tokens"]) != 0 {
		t.Fatalf("expected no tokens after deletion")
	}

	if tokensAfter, err := apiServer.loadAuthTokens(context.Background()); err != nil {
		t.Fatalf("failed to load tokens: %v", err)
	} else if len(tokensAfter) != 0 {
		t.Fatalf("expected no tokens after deletion, got %d", len(tokensAfter))
	}
}

func TestHTTPAuthEnforcedForLanBinding(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	tmpDir := t.TempDir()
	certPath, keyPath := writeSelfSignedCert(t, tmpDir)

	ctx := context.Background()
	if err := apiServer.configStore.SaveTransportConfig(ctx, configstore.TransportConfig{
		Port:        0,
		Binding:     "lan",
		TLSCertPath: certPath,
		TLSKeyPath:  keyPath,
	}); err != nil {
		t.Fatalf("failed to save transport config: %v", err)
	}

	tokens, newToken, err := apiServer.ensureAuthTokens(ctx, true)
	if err != nil {
		t.Fatalf("ensure auth tokens: %v", err)
	}
	token := newToken
	if token == "" && len(tokens) > 0 {
		token = tokens[0].Token
	}
	if token == "" {
		t.Fatal("expected a generated API token")
	}

	prepared, err := apiServer.Prepare(ctx)
	if err != nil {
		t.Fatalf("prepare server: %v", err)
	}

	handler := prepared.Server.Handler

	req := httptest.NewRequest(http.MethodGet, "/daemon/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d without token, got %d", http.StatusUnauthorized, rec.Code)
	}

	reqAuth := httptest.NewRequest(http.MethodGet, "/daemon/status", nil)
	reqAuth.Header.Set("Authorization", "Bearer "+token)
	recAuth := httptest.NewRecorder()
	handler.ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusOK {
		t.Fatalf("expected status %d with valid token, got %d", http.StatusOK, recAuth.Code)
	}
}

func TestHTTPAuthNotRequiredOnLoopback(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	prepared, err := apiServer.Prepare(context.Background())
	if err != nil {
		t.Fatalf("prepare server: %v", err)
	}

	handler := prepared.Server.Handler

	req := httptest.NewRequest(http.MethodGet, "/daemon/status", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d on loopback without token, got %d", http.StatusOK, rec.Code)
	}
}

func TestHandleQuickstartGet(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.SetModulesService(newTestModulesService(t, apiServer.configStore))

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/config/quickstart", nil))
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var payload quickstartStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if payload.Completed {
		t.Fatalf("expected quickstart to be incomplete by default")
	}
	if len(payload.PendingSlots) == 0 {
		t.Fatalf("expected pending slots to be populated for required bindings")
	}

	if !reflect.DeepEqual(payload.MissingReferenceAdapters, modules.RequiredReferenceAdapters) {
		t.Fatalf("expected missing reference adapters %v, got %v", modules.RequiredReferenceAdapters, payload.MissingReferenceAdapters)
	}
}

func TestHandleQuickstartIncludesModules(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore

	modulesSvc := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesSvc)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.quickstart", Source: "builtin", Type: "ai", Name: "Quickstart AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/config/quickstart", nil))
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var payload quickstartStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	if len(payload.Modules) == 0 {
		t.Fatalf("expected modules list in quickstart response")
	}

	if !reflect.DeepEqual(payload.MissingReferenceAdapters, modules.RequiredReferenceAdapters) {
		t.Fatalf("expected missing reference adapters %v, got %v", modules.RequiredReferenceAdapters, payload.MissingReferenceAdapters)
	}

	var found bool
	for _, entry := range payload.Modules {
		if entry.Slot == "ai.primary" {
			found = true
			if entry.AdapterID == nil || *entry.AdapterID != adapter.ID {
				t.Fatalf("expected adapter %s, got %v", adapter.ID, entry.AdapterID)
			}
			if strings.TrimSpace(entry.Status) == "" {
				t.Fatalf("expected status for module entry %+v", entry)
			}
			break
		}
	}
	if !found {
		t.Fatalf("ai.primary slot not present in modules overview")
	}
}

func TestHandleQuickstartWithoutModulesService(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/config/quickstart", nil))
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d when modules service unavailable, got %d", http.StatusServiceUnavailable, rec.Code)
	}
}

func TestHandleQuickstartCompleteValidation(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.SetModulesService(newTestModulesService(t, apiServer.configStore))

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/config/quickstart", bytes.NewBufferString(`{"complete":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d when completing with pending slots, got %d", http.StatusBadRequest, rec.Code)
	}
}

func TestHandleQuickstartCompleteFailsWhenReferenceMissing(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := apiServer.configStore
	apiServer.SetModulesService(newTestModulesService(t, store))

	ctx := context.Background()
	adapters := []configstore.Adapter{
		{ID: "adapter.ai.quick", Source: "builtin", Type: "ai", Name: "AI"},
		{ID: "adapter.stt.custom", Source: "builtin", Type: "stt", Name: "STT"},
		{ID: "adapter.stt.secondary", Source: "builtin", Type: "stt", Name: "STT Secondary"},
		{ID: "adapter.tts.custom", Source: "builtin", Type: "tts", Name: "TTS"},
		{ID: "adapter.vad.custom", Source: "builtin", Type: "vad", Name: "VAD"},
		{ID: "adapter.tunnel.custom", Source: "builtin", Type: "tunnel", Name: "Tunnel"},
	}
	for _, adapter := range adapters {
		if err := store.UpsertAdapter(ctx, adapter); err != nil {
			t.Fatalf("upsert adapter %s: %v", adapter.ID, err)
		}
	}

	bindings := map[string]string{
		"ai.primary":     "adapter.ai.quick",
		"stt.primary":    "adapter.stt.custom",
		"stt.secondary":  "adapter.stt.secondary",
		"tts.primary":    "adapter.tts.custom",
		"vad.primary":    "adapter.vad.custom",
		"tunnel.primary": "adapter.tunnel.custom",
	}
	for slot, adapterID := range bindings {
		if err := store.SetActiveAdapter(ctx, slot, adapterID, nil); err != nil {
			t.Fatalf("binding %s: %v", slot, err)
		}
	}

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/config/quickstart", bytes.NewBufferString(`{"complete":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d when reference adapters missing, got %d", http.StatusBadRequest, rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reference adapters missing") {
		t.Fatalf("expected reference missing message, got %s", rec.Body.String())
	}
}

func TestHandleMetricsUnavailable(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	handler := apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleMetrics))
	rec := httptest.NewRecorder()
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected status %d when exporter is missing, got %d", http.StatusServiceUnavailable, rec.Code)
	}
}

func TestHandleMetricsSuccess(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.SetMetricsExporter(metricsExporterStub{payload: []byte("metric_line\n")})

	handler := apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleMetrics))
	rec := httptest.NewRecorder()
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; version=0.0.4" {
		t.Fatalf("unexpected content type: %s", got)
	}
	if body := rec.Body.String(); body != "metric_line\n" {
		t.Fatalf("unexpected response body: %q", body)
	}
}

func TestMetricsEndpointAggregatesData(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	bus := eventbus.New()
	counter := observability.NewEventCounter()
	bus.AddObserver(counter)

	exporter := observability.NewPrometheusExporter(bus, counter)
	exporter.WithPipeline(pipelineMetricsStub{processed: 7, errors: 1})
	apiServer.SetMetricsExporter(exporter)

	bus.Publish(context.Background(), eventbus.Envelope{Topic: eventbus.TopicSessionsOutput})
	bus.Publish(context.Background(), eventbus.Envelope{Topic: eventbus.TopicPipelineCleaned})

	handler := apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleMetrics))
	rec := httptest.NewRecorder()
	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `nupi_eventbus_events_total{topic="pipeline.cleaned"} 1`) {
		t.Fatalf("expected pipeline.cleaned counter in metrics output:\n%s", body)
	}
	if !strings.Contains(body, `nupi_eventbus_events_total{topic="sessions.output"} 1`) {
		t.Fatalf("expected sessions.output counter in metrics output:\n%s", body)
	}
	if !strings.Contains(body, `nupi_pipeline_processed_total 7`) {
		t.Fatalf("expected pipeline processed metric in output:\n%s", body)
	}
	if !strings.Contains(body, `nupi_pipeline_errors_total 1`) {
		t.Fatalf("expected pipeline error metric in output:\n%s", body)
	}
}

func TestHandleMetricsUnauthorized(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	apiServer.authMu.Lock()
	apiServer.authRequired = true
	apiServer.authTokens = make(map[string]storedToken)
	apiServer.authMu.Unlock()

	handler := apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleMetrics))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d when no token provided, got %d", http.StatusUnauthorized, rec.Code)
	}
}

func TestIsPublicAuthEndpointExcludesMetrics(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if isPublicAuthEndpoint(req) {
		t.Fatalf("expected /metrics GET to require auth when auth is enabled")
	}

	postReq := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	if isPublicAuthEndpoint(postReq) {
		t.Fatalf("expected /metrics POST to require authentication")
	}
}

func TestHandleModulesGet(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	modulesService := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesService)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.primary", Source: "builtin", Type: "ai", Name: "Primary AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}
	if err := store.SetActiveAdapter(ctx, "ai.primary", adapter.ID, nil); err != nil {
		t.Fatalf("set active adapter: %v", err)
	}

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodGet, "/modules", nil))
	rec := httptest.NewRecorder()

	apiServer.handleModules(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, rec.Code)
	}

	var payload apihttp.ModulesOverview
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}

	var found bool
	for _, module := range payload.Modules {
		if module.Slot == "ai.primary" {
			found = true
			if module.AdapterID == nil || *module.AdapterID != adapter.ID {
				t.Fatalf("expected adapter %s, got %v", adapter.ID, module.AdapterID)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected ai.primary slot in response")
	}
}

func TestHandleModulesBindStartStop(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	store := openTestStore(t)
	modulesService := newTestModulesService(t, store)
	apiServer.SetModulesService(modulesService)

	ctx := context.Background()
	adapter := configstore.Adapter{ID: "adapter.ai.bind", Source: "builtin", Type: "ai", Name: "Bind AI"}
	if err := store.UpsertAdapter(ctx, adapter); err != nil {
		t.Fatalf("upsert adapter: %v", err)
	}

	bindReq := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/modules/bind", bytes.NewBufferString(`{"slot":"ai.primary","adapter_id":"adapter.ai.bind"}`)))
	bindReq.Header.Set("Content-Type", "application/json")
	bindRec := httptest.NewRecorder()

	apiServer.handleModulesBind(bindRec, bindReq)
	if bindRec.Code != http.StatusOK {
		t.Fatalf("bind expected status %d, got %d", http.StatusOK, bindRec.Code)
	}

	var bindPayload apihttp.ModuleActionResult
	if err := json.Unmarshal(bindRec.Body.Bytes(), &bindPayload); err != nil {
		t.Fatalf("invalid bind response: %v", err)
	}
	if bindPayload.Module.AdapterID == nil || *bindPayload.Module.AdapterID != adapter.ID {
		t.Fatalf("bind response adapter mismatch: %v", bindPayload.Module.AdapterID)
	}
	if bindPayload.Module.Status != configstore.BindingStatusActive {
		t.Fatalf("expected status %s, got %s", configstore.BindingStatusActive, bindPayload.Module.Status)
	}

	startReq := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/modules/start", bytes.NewBufferString(`{"slot":"ai.primary"}`)))
	startReq.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	apiServer.handleModulesStart(startRec, startReq)
	if startRec.Code != http.StatusOK {
		t.Fatalf("start expected status %d, got %d", http.StatusOK, startRec.Code)
	}

	var startPayload apihttp.ModuleActionResult
	if err := json.Unmarshal(startRec.Body.Bytes(), &startPayload); err != nil {
		t.Fatalf("invalid start response: %v", err)
	}
	if startPayload.Module.Status != configstore.BindingStatusActive {
		t.Fatalf("expected active status after start, got %s", startPayload.Module.Status)
	}
	if startPayload.Module.Runtime == nil || startPayload.Module.Runtime.Health == "" {
		t.Fatalf("expected runtime health after start")
	}

	stopReq := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/modules/stop", bytes.NewBufferString(`{"slot":"ai.primary"}`)))
	stopReq.Header.Set("Content-Type", "application/json")
	stopRec := httptest.NewRecorder()
	apiServer.handleModulesStop(stopRec, stopReq)
	if stopRec.Code != http.StatusOK {
		t.Fatalf("stop expected status %d, got %d", http.StatusOK, stopRec.Code)
	}

	var stopPayload apihttp.ModuleActionResult
	if err := json.Unmarshal(stopRec.Body.Bytes(), &stopPayload); err != nil {
		t.Fatalf("invalid stop response: %v", err)
	}
	if stopPayload.Module.Status != configstore.BindingStatusInactive {
		t.Fatalf("expected inactive status after stop, got %s", stopPayload.Module.Status)
	}
	if stopPayload.Module.Runtime == nil || !strings.EqualFold(stopPayload.Module.Runtime.Health, string(eventbus.ModuleHealthStopped)) {
		t.Fatalf("expected runtime health 'stopped', got %+v", stopPayload.Module.Runtime)
	}
}

type runtimeStub struct {
	port int
}

func (r runtimeStub) Port() int {
	return r.port
}

func (runtimeStub) GRPCPort() int {
	return 0
}

func (runtimeStub) StartTime() time.Time {
	return time.Unix(0, 0)
}

func newTestAPIServer(t *testing.T) (*APIServer, *session.Manager) {
	t.Helper()

	tmpHome := t.TempDir()
	oldHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", tmpHome); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}
	t.Cleanup(func() {
		os.Setenv("HOME", oldHome)
	})

	if _, err := config.EnsureInstanceDirs(config.DefaultInstance); err != nil {
		t.Fatalf("failed to ensure instance dirs: %v", err)
	}
	if _, err := config.EnsureProfileDirs(config.DefaultInstance, config.DefaultProfile); err != nil {
		t.Fatalf("failed to ensure profile dirs: %v", err)
	}

	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
	})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})

	sessionManager := session.NewManager()

	apiServer, err := NewAPIServer(sessionManager, store, runtimeStub{port: 9999}, 0)
	if err != nil {
		t.Fatalf("failed to create API server: %v", err)
	}

	return apiServer, sessionManager
}

func openTestStore(t *testing.T) *configstore.Store {
	t.Helper()
	store, err := configstore.Open(configstore.Options{InstanceName: config.DefaultInstance, ProfileName: config.DefaultProfile})
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() {
		store.Close()
	})
	return store
}

func newTestModulesService(t *testing.T, store *configstore.Store) *modules.Service {
	t.Helper()
	manager := modules.NewManager(modules.ManagerOptions{
		Store:    store,
		Runner:   adapterrunner.NewManager(t.TempDir()),
		Launcher: testModuleLauncher{},
	})
	bus := eventbus.New()
	return modules.NewService(manager, store, bus, modules.WithEnsureInterval(0))
}

type testModuleLauncher struct{}

func (testModuleLauncher) Launch(context.Context, string, []string, []string) (modules.ProcessHandle, error) {
	return testModuleHandle{}, nil
}

type testModuleHandle struct{}

func (testModuleHandle) Stop(context.Context) error { return nil }

func (testModuleHandle) PID() int { return 12345 }

func TestHandleAudioIngressStreamsData(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	apiServer.sessionManager = nil

	rawSub := bus.Subscribe(eventbus.TopicAudioIngressRaw)
	defer rawSub.Close()
	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)
	defer segSub.Close()

	payload := bytes.Repeat([]byte{0x01, 0x02}, 320)
	req := httptest.NewRequest(http.MethodPost, "/audio/ingress?session_id=sess&stream_id=mic&sample_rate=16000&channels=1&bit_depth=16&metadata=%7B%22client%22%3A%22web%22%7D", bytes.NewReader(payload))
	req = withAdmin(apiServer, req)

	rec := httptest.NewRecorder()
	apiServer.handleAudioIngress(rec, req)

	if rec.Result().StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", rec.Result().StatusCode)
	}

	rawEvtEnv := recvEvent(t, rawSub)
	rawEvt, ok := rawEvtEnv.Payload.(eventbus.AudioIngressRawEvent)
	if !ok {
		t.Fatalf("unexpected raw payload %T", rawEvtEnv.Payload)
	}
	if rawEvt.SessionID != "sess" || rawEvt.StreamID != "mic" {
		t.Fatalf("unexpected raw identifiers: %+v", rawEvt)
	}
	if len(rawEvt.Data) != len(payload) {
		t.Fatalf("unexpected raw size: %d", len(rawEvt.Data))
	}
	if rawEvt.Metadata["client"] != "web" {
		t.Fatalf("metadata not propagated: %+v", rawEvt.Metadata)
	}

	var (
		finalSeg eventbus.AudioIngressSegmentEvent
		observed []time.Duration
	)
	for {
		env := recvEvent(t, segSub)
		segEvt, ok := env.Payload.(eventbus.AudioIngressSegmentEvent)
		if !ok {
			t.Fatalf("unexpected segment payload %T", env.Payload)
		}
		observed = append(observed, segEvt.Duration)
		if segEvt.Last {
			finalSeg = segEvt
			break
		}
	}
	if len(observed) == 0 || observed[0] <= 0 {
		t.Fatalf("expected positive segment duration, got %v", observed)
	}
	if finalSeg.Metadata["client"] != "web" {
		t.Fatalf("metadata missing on final segment: %+v", finalSeg.Metadata)
	}
}

func TestHandleAudioEgressStreamsChunks(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	egressSvc := egress.New(bus)
	apiServer.SetAudioEgressService(egressSvc)
	apiServer.sessionManager = nil

	req := httptest.NewRequest(http.MethodGet, "/audio/egress?session_id=sess", nil)
	req = withReadOnly(apiServer, req)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		apiServer.handleAudioEgress(rec, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)

	format := egressSvc.PlaybackFormat()
	streamID := egressSvc.DefaultStreamID()

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess",
			StreamID:  streamID,
			Sequence:  1,
			Format:    format,
			Duration:  150 * time.Millisecond,
			Data:      []byte{0x01, 0x02, 0x03, 0x04},
			Final:     false,
			Metadata:  map[string]string{"phase": "speak"},
		},
	})

	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess",
			StreamID:  streamID,
			Sequence:  2,
			Format:    format,
			Duration:  0,
			Data:      []byte{},
			Final:     true,
			Metadata:  map[string]string{"barge_in": "true"},
		},
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for audio egress handler")
	}

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content-type: %s", ct)
	}

	scanner := bufio.NewScanner(res.Body)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(lines))
	}

	var first audioEgressHTTPChunk
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal first chunk: %v", err)
	}
	if first.Format == nil || first.Format.SampleRate != format.SampleRate {
		t.Fatalf("unexpected format %+v", first.Format)
	}
	if first.Sequence != 1 || first.Final {
		t.Fatalf("unexpected first chunk fields: %+v", first)
	}
	if first.DurationMs != 150 {
		t.Fatalf("unexpected first chunk duration: %d", first.DurationMs)
	}
	data, err := base64.StdEncoding.DecodeString(first.Data)
	if err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("unexpected data length: %d", len(data))
	}

	var second audioEgressHTTPChunk
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("unmarshal second chunk: %v", err)
	}
	if !second.Final || second.DurationMs != 0 {
		t.Fatalf("unexpected second chunk: %+v", second)
	}
	if second.Metadata["barge_in"] != "true" {
		t.Fatalf("barge metadata missing: %+v", second.Metadata)
	}
}

func recvEvent(t *testing.T, sub *eventbus.Subscription) eventbus.Envelope {
	select {
	case env := <-sub.C():
		return env
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
	return eventbus.Envelope{}
}

func TestHandleAudioIngressRejectsInvalidFormat(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	apiServer.sessionManager = nil

	req := httptest.NewRequest(http.MethodPost, "/audio/ingress?session_id=sess&sample_rate=abc", bytes.NewReader([]byte{0x00}))
	req = withAdmin(apiServer, req)
	resp := httptest.NewRecorder()
	apiServer.handleAudioIngress(resp, req)

	if resp.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected bad request, got %d", resp.Result().StatusCode)
	}
}

func TestHandleAudioEgressSignalsError(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.sessionManager = nil

	req := httptest.NewRequest(http.MethodGet, "/audio/egress?session_id=sess", nil)
	req = withReadOnly(apiServer, req)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		apiServer.handleAudioEgress(rec, req)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	bus.Shutdown()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for error signalling")
	}

	l := bufio.NewScanner(rec.Result().Body)
	for l.Scan() {
		var chunk audioEgressHTTPChunk
		if err := json.Unmarshal(l.Bytes(), &chunk); err != nil {
			t.Fatalf("unmarshal chunk: %v", err)
		}
		if chunk.Error != "" {
			if !chunk.Final {
				t.Fatalf("error chunk should be final: %+v", chunk)
			}
			return
		}
	}
	if err := l.Err(); err != nil {
		t.Fatalf("scan body: %v", err)
	}
	t.Fatal("expected error chunk not found")
}

func TestHandleAudioIngressRejectsLargeMetadata(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	apiServer.sessionManager = nil

	bigValue := strings.Repeat("a", maxMetadataTotalPayload+1)
	metadata := fmt.Sprintf("{\"extra\":\"%s\"}", bigValue)
	req := httptest.NewRequest(http.MethodPost, "/audio/ingress?session_id=sess&metadata="+url.QueryEscape(metadata), bytes.NewReader([]byte{0x00}))
	req = withAdmin(apiServer, req)
	resp := httptest.NewRecorder()
	apiServer.handleAudioIngress(resp, req)

	if resp.Result().StatusCode != http.StatusBadRequest {
		t.Fatalf("expected bad request for large metadata, got %d", resp.Result().StatusCode)
	}
}

func TestHandleAudioIngressWebSocket(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	apiServer.sessionManager = nil

	token := newStoredToken("ws-admin-token", "ws-admin", string(roleAdmin))
	apiServer.setAuthTokens([]storedToken{token}, true)

	mux := http.NewServeMux()
	mux.Handle("/audio/ingress/ws", apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleAudioIngressWS)))
	ts := newLocalHTTPServer(t, mux)
	if ts == nil {
		t.Skip("tcp listener unavailable in this environment")
		return
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token.Token)
	header.Set("Origin", "http://localhost")
	url := strings.Replace(ts.URL, "http", "ws", 1) + "/audio/ingress/ws?session_id=sess"
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	rawSub := bus.Subscribe(eventbus.TopicAudioIngressRaw)
	defer rawSub.Close()
	segSub := bus.Subscribe(eventbus.TopicAudioIngressSegment)
	defer segSub.Close()

	payload := []byte{1, 2, 3, 4}
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	if err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "bye")); err != nil {
		// ignore close errors
	}

	rawEnv := recvEvent(t, rawSub)
	rawEvt := rawEnv.Payload.(eventbus.AudioIngressRawEvent)
	if !bytes.Equal(rawEvt.Data, payload) {
		t.Fatalf("unexpected raw data: %v", rawEvt.Data)
	}

	segEnv := recvEvent(t, segSub)
	segEvt := segEnv.Payload.(eventbus.AudioIngressSegmentEvent)
	if segEvt.SessionID != "sess" {
		t.Fatalf("unexpected segment session: %s", segEvt.SessionID)
	}
}

func TestHandleAudioEgressWebSocket(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	egressSvc := egress.New(bus)
	apiServer.SetAudioEgressService(egressSvc)
	apiServer.sessionManager = nil

	token := newStoredToken("ws-read-token", "ws-read", string(roleReadOnly))
	apiServer.setAuthTokens([]storedToken{token}, true)

	mux := http.NewServeMux()
	mux.Handle("/audio/egress/ws", apiServer.wrapWithSecurity(http.HandlerFunc(apiServer.handleAudioEgressWS)))
	ts := newLocalHTTPServer(t, mux)
	if ts == nil {
		t.Skip("tcp listener unavailable in this environment")
		return
	}

	header := http.Header{}
	header.Set("Authorization", "Bearer "+token.Token)
	header.Set("Origin", "http://localhost")
	url := strings.Replace(ts.URL, "http", "ws", 1) + "/audio/egress/ws?session_id=sess"
	conn, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	format := egressSvc.PlaybackFormat()
	streamID := egressSvc.DefaultStreamID()
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess",
			StreamID:  streamID,
			Sequence:  1,
			Format:    format,
			Duration:  100 * time.Millisecond,
			Data:      []byte{9, 8, 7, 6},
			Final:     false,
		},
	})
	bus.Publish(context.Background(), eventbus.Envelope{
		Topic: eventbus.TopicAudioEgressPlayback,
		Payload: eventbus.AudioEgressPlaybackEvent{
			SessionID: "sess",
			StreamID:  streamID,
			Sequence:  2,
			Format:    format,
			Duration:  0,
			Data:      []byte{},
			Final:     true,
		},
	})

	_, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	var first audioEgressHTTPChunk
	if err := json.Unmarshal(msg, &first); err != nil {
		t.Fatalf("unmarshal first chunk: %v", err)
	}
	if first.Sequence != 1 || first.Format == nil {
		t.Fatalf("unexpected first chunk: %+v", first)
	}

	_, msg, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second chunk: %v", err)
	}
	var second audioEgressHTTPChunk
	if err := json.Unmarshal(msg, &second); err != nil {
		t.Fatalf("unmarshal second chunk: %v", err)
	}
	if !second.Final {
		t.Fatalf("expected final chunk: %+v", second)
	}
}

func TestHandleAudioCapabilities(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	ingressSvc := ingress.New(bus)
	apiServer.SetAudioIngressService(ingressSvc)
	egressSvc := egress.New(bus)
	apiServer.SetAudioEgressService(egressSvc)

	req := httptest.NewRequest(http.MethodGet, "/audio/capabilities", nil)
	req = withReadOnly(apiServer, req)
	rec := httptest.NewRecorder()

	apiServer.handleAudioCapabilities(rec, req)

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", res.StatusCode, rec.Body.String())
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("unexpected content-type: %s", ct)
	}

	var payload audioCapabilitiesResponse
	if err := json.NewDecoder(res.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(payload.Capture) != 1 {
		t.Fatalf("expected capture capability, got %d", len(payload.Capture))
	}
	capCapture := payload.Capture[0]
	if capCapture.StreamID != defaultCaptureStreamID {
		t.Fatalf("unexpected capture stream id: %s", capCapture.StreamID)
	}
	if capCapture.Format.SampleRate != defaultCaptureFormat.SampleRate {
		t.Fatalf("unexpected capture sample rate: %d", capCapture.Format.SampleRate)
	}
	if int(capCapture.Format.Channels) != defaultCaptureFormat.Channels {
		t.Fatalf("unexpected capture channels: %d", capCapture.Format.Channels)
	}
	if capCapture.Format.BitDepth != defaultCaptureFormat.BitDepth {
		t.Fatalf("unexpected capture bit depth: %d", capCapture.Format.BitDepth)
	}
	expectedCaptureFrame := uint32(defaultCaptureFormat.FrameDuration / time.Millisecond)
	if capCapture.Format.FrameDurationMs != expectedCaptureFrame {
		t.Fatalf("unexpected capture frame duration: %d", capCapture.Format.FrameDurationMs)
	}
	if capCapture.Metadata["recommended"] != "true" {
		t.Fatalf("expected capture metadata recommended=true, got %+v", capCapture.Metadata)
	}

	if len(payload.Playback) != 1 {
		t.Fatalf("expected playback capability, got %d", len(payload.Playback))
	}
	playbackFormat := egressSvc.PlaybackFormat()
	playbackStream := egressSvc.DefaultStreamID()
	capPlayback := payload.Playback[0]
	if capPlayback.StreamID != playbackStream {
		t.Fatalf("unexpected playback stream id: %s", capPlayback.StreamID)
	}
	if capPlayback.Format.SampleRate != playbackFormat.SampleRate {
		t.Fatalf("unexpected playback sample rate: %d", capPlayback.Format.SampleRate)
	}
	if int(capPlayback.Format.Channels) != playbackFormat.Channels {
		t.Fatalf("unexpected playback channels: %d", capPlayback.Format.Channels)
	}
	if capPlayback.Format.BitDepth != playbackFormat.BitDepth {
		t.Fatalf("unexpected playback bit depth: %d", capPlayback.Format.BitDepth)
	}
	expectedPlaybackFrame := uint32(playbackFormat.FrameDuration / time.Millisecond)
	if capPlayback.Format.FrameDurationMs != expectedPlaybackFrame {
		t.Fatalf("unexpected playback frame duration: %d", capPlayback.Format.FrameDurationMs)
	}
	if capPlayback.Metadata["recommended"] != "true" {
		t.Fatalf("expected playback metadata recommended=true, got %+v", capPlayback.Metadata)
	}
}

func TestHandleAudioCapabilitiesValidatesSession(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)
	bus := eventbus.New()
	apiServer.SetEventBus(bus)
	apiServer.SetAudioIngressService(ingress.New(bus))

	req := httptest.NewRequest(http.MethodGet, "/audio/capabilities?session_id=missing", nil)
	req = withReadOnly(apiServer, req)
	rec := httptest.NewRecorder()

	apiServer.handleAudioCapabilities(rec, req)

	if rec.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for missing session, got %d", rec.Result().StatusCode)
	}
}

type localHTTPServer struct {
	URL      string
	server   *http.Server
	listener net.Listener
}

func newLocalHTTPServer(t *testing.T, handler http.Handler) *localHTTPServer {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("skipping websocket test; listen tcp4 not permitted: %v", err)
		return nil
	}
	srv := &http.Server{Handler: handler}
	out := &localHTTPServer{
		URL:      "http://" + ln.Addr().String(),
		server:   srv,
		listener: ln,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	t.Cleanup(out.Close)
	return out
}

func (s *localHTTPServer) Close() {
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
	_ = s.listener.Close()
}

type metricsExporterStub struct {
	payload []byte
}

func (m metricsExporterStub) Export() []byte {
	return m.payload
}

type pipelineMetricsStub struct {
	processed uint64
	errors    uint64
}

func (p pipelineMetricsStub) Metrics() contentpipeline.Metrics {
	return contentpipeline.Metrics{
		Processed: p.processed,
		Errors:    p.errors,
	}
}
