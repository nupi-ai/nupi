package client

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/config"
	configstore "github.com/nupi-ai/nupi/internal/config/store"
)

// LoadTransportSettings loads transport config and API tokens from the config store.
func LoadTransportSettings() (configstore.TransportConfig, []string, error) {
	store, err := configstore.Open(configstore.Options{
		InstanceName: config.DefaultInstance,
		ProfileName:  config.DefaultProfile,
		ReadOnly:     true,
	})
	if err != nil {
		return configstore.TransportConfig{}, nil, fmt.Errorf("client: open config store: %w", err)
	}
	defer store.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cfg, err := store.GetTransportConfig(ctx)
	if err != nil {
		return configstore.TransportConfig{}, nil, fmt.Errorf("client: load transport config: %w", err)
	}

	security, err := store.LoadSecuritySettings(ctx, "auth.http_tokens")
	if err != nil {
		return cfg, nil, fmt.Errorf("client: load security settings: %w", err)
	}

	tokens := parseTokens(security["auth.http_tokens"])
	return cfg, tokens, nil
}

func parseTokens(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var simple []string
	if err := json.Unmarshal([]byte(raw), &simple); err == nil {
		return sanitizeTokenList(simple)
	}

	var structured []map[string]any
	if err := json.Unmarshal([]byte(raw), &structured); err == nil {
		collected := make([]string, 0, len(structured))
		for _, entry := range structured {
			if token, ok := entry["token"].(string); ok {
				token = strings.TrimSpace(token)
				if token != "" {
					collected = append(collected, token)
				}
			}
		}
		return sanitizeTokenList(collected)
	}
	return nil
}

func sanitizeTokenList(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(tokens))
	out := make([]string, 0, len(tokens))
	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		if _, ok := seen[token]; ok {
			continue
		}
		seen[token] = struct{}{}
		out = append(out, token)
	}
	return out
}
