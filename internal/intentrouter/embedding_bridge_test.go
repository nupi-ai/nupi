package intentrouter

import (
	"context"
	"strings"
	"testing"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// mockEmbeddingClient implements the GenerateEmbeddings method of
// napv1.IntentResolutionServiceClient for testing.
type mockEmbeddingClient struct {
	napv1.IntentResolutionServiceClient // embed for other methods
	resp                                *napv1.EmbeddingResponse
	err                                 error
}

func (m *mockEmbeddingClient) GenerateEmbeddings(ctx context.Context, in *napv1.EmbeddingRequest, opts ...grpc.CallOption) (*napv1.EmbeddingResponse, error) {
	return m.resp, m.err
}

func TestEmbeddingBridgeSuccess(t *testing.T) {
	mock := &mockEmbeddingClient{
		resp: &napv1.EmbeddingResponse{
			Embeddings: []*napv1.EmbeddingVector{
				{Values: []float32{0.1, 0.2, 0.3}},
				{Values: []float32{0.4, 0.5, 0.6}},
			},
			ModelUsed:  "text-embedding-3-small",
			Dimensions: 3,
		},
	}

	b := NewEmbeddingBridge()
	b.SetClient(mock)

	result, err := b.GenerateEmbeddings(context.Background(), []string{"hello", "world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Dimensions != 3 {
		t.Fatalf("expected dimensions=3, got %d", result.Dimensions)
	}
	if len(result.Vectors) != 2 {
		t.Fatalf("expected 2 vectors, got %d", len(result.Vectors))
	}
	if result.Vectors[0][0] != 0.1 || result.Vectors[0][1] != 0.2 || result.Vectors[0][2] != 0.3 {
		t.Fatalf("unexpected vector[0]: %v", result.Vectors[0])
	}
	if result.Vectors[1][0] != 0.4 || result.Vectors[1][1] != 0.5 || result.Vectors[1][2] != 0.6 {
		t.Fatalf("unexpected vector[1]: %v", result.Vectors[1])
	}
	if result.Model != "text-embedding-3-small" {
		t.Fatalf("expected model 'text-embedding-3-small', got %q", result.Model)
	}
}

func TestEmbeddingBridgeErrorInResponse(t *testing.T) {
	mock := &mockEmbeddingClient{
		resp: &napv1.EmbeddingResponse{
			ErrorMessage: "model overloaded",
		},
	}

	b := NewEmbeddingBridge()
	b.SetClient(mock)

	_, err := b.GenerateEmbeddings(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := err.Error(); got != "embedding: provider error: model overloaded" {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestEmbeddingBridgeGRPCError(t *testing.T) {
	mock := &mockEmbeddingClient{
		err: status.Error(codes.Unavailable, "connection refused"),
	}

	b := NewEmbeddingBridge()
	b.SetClient(mock)

	_, err := b.GenerateEmbeddings(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// Should contain "gRPC error"
	if got := err.Error(); !strings.Contains(got, "gRPC error") {
		t.Fatalf("expected gRPC error, got: %s", got)
	}
}

func TestEmbeddingBridgeNilClient(t *testing.T) {
	b := NewEmbeddingBridge()
	// No SetClient called — client is nil

	_, err := b.GenerateEmbeddings(context.Background(), []string{"hello"})
	if err == nil {
		t.Fatal("expected error for nil client, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "no AI adapter connected") {
		t.Fatalf("unexpected error message: %s", got)
	}
}

func TestEmbeddingBridgeEmptyInput(t *testing.T) {
	mock := &mockEmbeddingClient{
		resp: &napv1.EmbeddingResponse{},
	}

	b := NewEmbeddingBridge()
	b.SetClient(mock)

	result, err := b.GenerateEmbeddings(context.Background(), []string{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vectors != nil {
		t.Fatalf("expected nil vectors for empty input, got %v", result.Vectors)
	}
	if result.Dimensions != 0 {
		t.Fatalf("expected dims=0 for empty input, got %d", result.Dimensions)
	}
}

func TestEmbeddingBridgeSetClientUpdatesReference(t *testing.T) {
	mock1 := &mockEmbeddingClient{
		resp: &napv1.EmbeddingResponse{
			Embeddings: []*napv1.EmbeddingVector{{Values: []float32{1.0}}},
			Dimensions: 1,
		},
	}
	mock2 := &mockEmbeddingClient{
		resp: &napv1.EmbeddingResponse{
			Embeddings: []*napv1.EmbeddingVector{{Values: []float32{2.0}}},
			Dimensions: 1,
		},
	}

	b := NewEmbeddingBridge()

	// Set first client
	b.SetClient(mock1)
	result, err := b.GenerateEmbeddings(context.Background(), []string{"test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vectors[0][0] != 1.0 {
		t.Fatalf("expected vector=1.0 from mock1, got %v", result.Vectors[0][0])
	}

	// Swap to second client
	b.SetClient(mock2)
	result, err = b.GenerateEmbeddings(context.Background(), []string{"test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Vectors[0][0] != 2.0 {
		t.Fatalf("expected vector=2.0 from mock2, got %v", result.Vectors[0][0])
	}

	// Clear client
	b.SetClient(nil)
	_, err = b.GenerateEmbeddings(context.Background(), []string{"test"})
	if err == nil {
		t.Fatal("expected error after clearing client, got nil")
	}
}

