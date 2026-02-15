package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	apihttp "github.com/nupi-ai/nupi/internal/api/http"
	"github.com/nupi-ai/nupi/internal/client"
)

// Shared types used across command files.

type transportConfigResponse struct {
	Port           int      `json:"port"`
	Binding        string   `json:"binding"`
	TLSCertPath    string   `json:"tls_cert_path,omitempty"`
	TLSKeyPath     string   `json:"tls_key_path,omitempty"`
	AllowedOrigins []string `json:"allowed_origins"`
	GRPCPort       int      `json:"grpc_port"`
	GRPCBinding    string   `json:"grpc_binding"`
	AuthRequired   bool     `json:"auth_required"`
}

type quickstartStatusPayload struct {
	Completed                bool                   `json:"completed"`
	CompletedAt              string                 `json:"completed_at,omitempty"`
	PendingSlots             []string               `json:"pending_slots"`
	Adapters                 []apihttp.AdapterEntry `json:"adapters,omitempty"`
	MissingReferenceAdapters []string               `json:"missing_reference_adapters,omitempty"`
}

type quickstartBindingRequest struct {
	Slot      string `json:"slot"`
	AdapterID string `json:"adapter_id"`
}

type adapterActionRequestPayload struct {
	Slot      string          `json:"slot"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type adapterInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Source  string `json:"source"`
	Version string `json:"version"`
}

const (
	quickstartMissingRefsWarning = "WARN: Missing reference adapters: %s\n"
	quickstartMissingRefsHelp    = "Install the recommended packages before completing quickstart.\n"
)

// HTTP helpers used across command files.

func resolveDaemonBaseURL(c *client.Client) (string, map[string]interface{}, error) {
	status, err := c.GetDaemonStatus()
	if err != nil {
		return "", nil, err
	}

	return c.BaseURL(), status, nil
}

func doRequest(c *client.Client, req *http.Request) (*http.Response, error) {
	if token := strings.TrimSpace(c.Token()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.HTTPClient().Do(req)
}

func doStreamingRequest(c *client.Client, req *http.Request) (*http.Response, error) {
	if token := strings.TrimSpace(c.Token()); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.StreamingHTTPClient().Do(req)
}

func readErrorMessage(resp *http.Response) string {
	limited := io.LimitReader(resp.Body, errorMessageLimit)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) == 0 {
		return strings.TrimSpace(resp.Status)
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(data, &errResp) == nil && errResp.Error != "" {
		return errResp.Error
	}
	return strings.TrimSpace(string(data))
}

func fetchQuickstartStatus(c *client.Client) (quickstartStatusPayload, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/config/quickstart", nil)
	if err != nil {
		return quickstartStatusPayload{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return quickstartStatusPayload{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return quickstartStatusPayload{}, errors.New(readErrorMessage(resp))
	}

	var payload quickstartStatusPayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return quickstartStatusPayload{}, err
	}

	return payload, nil
}

func runConfigMigration(c *client.Client) (apihttp.ConfigMigrationResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+"/config/migrate", nil)
	if err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.ConfigMigrationResult{}, errors.New(readErrorMessage(resp))
	}

	var payload apihttp.ConfigMigrationResult
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return apihttp.ConfigMigrationResult{}, err
	}

	return payload, nil
}

func fetchAdaptersOverview(c *client.Client) (apihttp.AdaptersOverview, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/adapters", nil)
	if err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.AdaptersOverview{}, errors.New(readErrorMessage(resp))
	}

	var payload apihttp.AdaptersOverview
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return apihttp.AdaptersOverview{}, err
	}
	return payload, nil
}

func fetchAdapters(c *client.Client) ([]adapterInfo, error) {
	req, err := http.NewRequest(http.MethodGet, c.BaseURL()+"/config/adapters", nil)
	if err != nil {
		return nil, err
	}
	resp, err := doRequest(c, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(readErrorMessage(resp))
	}

	var payload struct {
		Adapters []adapterInfo `json:"adapters"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	return payload.Adapters, nil
}

func postAdapterAction(c *client.Client, endpoint string, payload adapterActionRequestPayload) (apihttp.AdapterEntry, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}

	req, err := http.NewRequest(http.MethodPost, c.BaseURL()+endpoint, bytes.NewReader(body))
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := doRequest(c, req)
	if err != nil {
		return apihttp.AdapterEntry{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apihttp.AdapterEntry{}, errors.New(readErrorMessage(resp))
	}

	var actionResp apihttp.AdapterActionResult
	if err := json.NewDecoder(resp.Body).Decode(&actionResp); err != nil {
		return apihttp.AdapterEntry{}, err
	}
	return actionResp.Adapter, nil
}

func printMissingReferenceAdapters(missing []string, showHelp bool) {
	if len(missing) == 0 {
		return
	}
	fmt.Printf(quickstartMissingRefsWarning, strings.Join(missing, ", "))
	if showHelp {
		fmt.Print(quickstartMissingRefsHelp)
	}
}

func adapterTypeForSlot(slot string) string {
	slot = strings.TrimSpace(slot)
	if slot == "" {
		return ""
	}
	if idx := strings.IndexRune(slot, '.'); idx >= 0 {
		if idx == 0 {
			return ""
		}
		slot = slot[:idx]
	}
	return strings.ToLower(slot)
}

func filterAdaptersForSlot(slot string, adapters []adapterInfo) []adapterInfo {
	expectedType := adapterTypeForSlot(slot)
	if expectedType == "" {
		return nil
	}
	filtered := make([]adapterInfo, 0, len(adapters))
	for _, adapter := range adapters {
		if strings.EqualFold(adapter.Type, expectedType) {
			filtered = append(filtered, adapter)
		}
	}
	return filtered
}
