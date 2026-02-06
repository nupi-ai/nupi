package adapterutil

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nupi-ai/nupi/internal/plugins/adapters"
)

// RuntimeLookupTimeout is the default timeout for runtime address lookups.
const RuntimeLookupTimeout = time.Second

// AdapterRuntimeSource exposes runtime status for adapters.
type AdapterRuntimeSource interface {
	Overview(ctx context.Context) ([]adapters.BindingStatus, error)
}

// LookupRuntimeAddress resolves the gRPC listen address for a process-transport
// adapter by querying the runtime source. The prefix parameter (e.g. "vad",
// "stt", "tts") is used in error messages. errUnavailable is the
// package-specific sentinel error to wrap transient failures with.
func LookupRuntimeAddress(ctx context.Context, runtime AdapterRuntimeSource, adapterID, prefix string, errUnavailable error) (string, error) {
	if runtime == nil {
		return "", fmt.Errorf("%s: process adapter %s missing runtime metadata", prefix, adapterID)
	}
	ctxLookup, cancel := context.WithTimeout(ctx, RuntimeLookupTimeout)
	defer cancel()

	statuses, err := runtime.Overview(ctxLookup)
	if err != nil {
		return "", fmt.Errorf("%s: fetch adapter runtime: %w: %w", prefix, err, errUnavailable)
	}

	var (
		match    bool
		multiple bool
		address  string
	)
	for _, status := range statuses {
		if status.AdapterID == nil || strings.TrimSpace(*status.AdapterID) != adapterID {
			continue
		}
		if match {
			multiple = true
		}
		match = true
		if address == "" && status.Runtime != nil && len(status.Runtime.Extra) > 0 {
			address = strings.TrimSpace(status.Runtime.Extra[adapters.RuntimeExtraAddress])
		}
	}
	if multiple {
		return "", fmt.Errorf("%s: process adapter %s has duplicate runtime entries", prefix, adapterID)
	}
	if address != "" {
		return address, nil
	}
	if match {
		return "", fmt.Errorf("%s: process adapter %s awaiting runtime address: %w", prefix, adapterID, errUnavailable)
	}
	return "", fmt.Errorf("%s: process adapter %s not running: %w", prefix, adapterID, errUnavailable)
}
