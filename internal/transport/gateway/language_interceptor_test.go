package gateway

import (
	"context"
	"testing"

	"github.com/nupi-ai/nupi/internal/language"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestExtractLanguageToContext_ValidCode(t *testing.T) {
	md := metadata.Pairs("nupi-language", "pl")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	newCtx, err := extractLanguageToContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lang, ok := language.FromContext(newCtx)
	if !ok {
		t.Fatal("language not found in context")
	}
	if lang.ISO1 != "pl" {
		t.Errorf("ISO1: got %q, want %q", lang.ISO1, "pl")
	}
	if lang.BCP47 != "pl-PL" {
		t.Errorf("BCP47: got %q, want %q", lang.BCP47, "pl-PL")
	}
	if lang.EnglishName != "Polish" {
		t.Errorf("EnglishName: got %q, want %q", lang.EnglishName, "Polish")
	}
	if lang.NativeName != "Polski" {
		t.Errorf("NativeName: got %q, want %q", lang.NativeName, "Polski")
	}
}

func TestExtractLanguageToContext_InvalidCode(t *testing.T) {
	md := metadata.Pairs("nupi-language", "xx")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	_, err := extractLanguageToContext(ctx)
	if err == nil {
		t.Fatal("expected error for invalid code")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got %v", err)
	}
	if st.Code() != codes.InvalidArgument {
		t.Errorf("expected InvalidArgument, got %v", st.Code())
	}
}

func TestExtractLanguageToContext_NoHeader(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{})

	newCtx, err := extractLanguageToContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, ok := language.FromContext(newCtx)
	if ok {
		t.Error("language should not be present when no header sent")
	}
}

func TestExtractLanguageToContext_NoMetadata(t *testing.T) {
	newCtx, err := extractLanguageToContext(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, ok := language.FromContext(newCtx)
	if ok {
		t.Error("language should not be present when no metadata")
	}
}

func TestExtractLanguageToContext_EmptyValue(t *testing.T) {
	md := metadata.Pairs("nupi-language", "")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	newCtx, err := extractLanguageToContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, ok := language.FromContext(newCtx)
	if ok {
		t.Error("language should not be present for empty value")
	}
}

func TestExtractLanguageToContext_WhitespaceValue(t *testing.T) {
	md := metadata.Pairs("nupi-language", "  en  ")
	ctx := metadata.NewIncomingContext(context.Background(), md)

	newCtx, err := extractLanguageToContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	lang, ok := language.FromContext(newCtx)
	if !ok {
		t.Fatal("language not found in context")
	}
	if lang.ISO1 != "en" {
		t.Errorf("ISO1: got %q, want %q", lang.ISO1, "en")
	}
}
