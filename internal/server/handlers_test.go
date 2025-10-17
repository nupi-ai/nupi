package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
	"github.com/nupi-ai/nupi/internal/eventbus"
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
		SessionID string `json:"session_id"`
		Turns     []struct {
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
}

func TestHandleQuickstartCompleteValidation(t *testing.T) {
	apiServer, _ := newTestAPIServer(t)

	req := withAdmin(apiServer, httptest.NewRequest(http.MethodPost, "/config/quickstart", bytes.NewBufferString(`{"complete":true}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	apiServer.handleQuickstart(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status %d when completing with pending slots, got %d", http.StatusBadRequest, rec.Code)
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
