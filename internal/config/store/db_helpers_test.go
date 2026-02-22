package store

import (
	"database/sql"
	"testing"
)

func TestEncodeJSONPreservesNullSemantics(t *testing.T) {
	t.Parallel()

	payload, err := encodeJSON([]string(nil), nullWhenEmptySlice[string])
	if err != nil {
		t.Fatalf("encode nil slice: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload for nil slice, got %T(%v)", payload, payload)
	}

	payload, err = encodeJSON([]string{}, nullWhenEmptySlice[string])
	if err != nil {
		t.Fatalf("encode empty slice: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload for empty slice, got %T(%v)", payload, payload)
	}

	payload, err = encodeJSON([]string{"one"}, nullWhenEmptySlice[string])
	if err != nil {
		t.Fatalf("encode non-empty slice: %v", err)
	}
	if payload != `["one"]` {
		t.Fatalf("expected JSON payload for non-empty slice, got %v", payload)
	}

	payload, err = encodeJSON(map[string]string{}, nullWhenEmptyMap[string, string])
	if err != nil {
		t.Fatalf("encode empty map with empty-map null rule: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload for empty map, got %T(%v)", payload, payload)
	}

	payload, err = encodeJSON(map[string]any(nil), nullWhenNilMap[string, any])
	if err != nil {
		t.Fatalf("encode nil config map: %v", err)
	}
	if payload != nil {
		t.Fatalf("expected nil payload for nil config map, got %T(%v)", payload, payload)
	}

	payload, err = encodeJSON(map[string]any{}, nullWhenNilMap[string, any])
	if err != nil {
		t.Fatalf("encode empty config map: %v", err)
	}
	if payload != "{}" {
		t.Fatalf("expected empty object JSON for empty config map, got %v", payload)
	}
}

func TestDecodeJSONPreservesEmptyCollectionSemantics(t *testing.T) {
	t.Parallel()

	sliceOut, err := decodeJSON[[]string](sql.NullString{})
	if err != nil {
		t.Fatalf("decode invalid nullstring slice: %v", err)
	}
	if sliceOut != nil {
		t.Fatalf("expected nil slice for invalid nullstring, got %v", sliceOut)
	}

	mapOut, err := decodeJSON[map[string]string](sql.NullString{Valid: true, String: "   "})
	if err != nil {
		t.Fatalf("decode blank string map: %v", err)
	}
	if mapOut != nil {
		t.Fatalf("expected nil map for blank string, got %v", mapOut)
	}

	sliceOut, err = decodeJSON[[]string](sql.NullString{Valid: true, String: "[]"})
	if err != nil {
		t.Fatalf("decode empty JSON array: %v", err)
	}
	if sliceOut == nil || len(sliceOut) != 0 {
		t.Fatalf("expected non-nil empty slice, got %#v", sliceOut)
	}

	mapOut, err = decodeJSON[map[string]string](sql.NullString{Valid: true, String: "{}"})
	if err != nil {
		t.Fatalf("decode empty JSON object: %v", err)
	}
	if mapOut == nil || len(mapOut) != 0 {
		t.Fatalf("expected non-nil empty map, got %#v", mapOut)
	}
}
