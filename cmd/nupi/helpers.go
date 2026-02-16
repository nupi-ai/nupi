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

type adapterActionRequestPayload struct {
	Slot      string          `json:"slot"`
	AdapterID string          `json:"adapter_id,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

// HTTP helpers used by remaining HTTP-only paths (register, install-local, logs, config migrate).

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
