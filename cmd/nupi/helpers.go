package main

import (
	"fmt"
	"strings"
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
