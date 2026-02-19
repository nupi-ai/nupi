package intentrouter

import (
	"context"
	"fmt"
	"sync"

	napv1 "github.com/nupi-ai/nupi/api/nap/v1"
	"github.com/nupi-ai/nupi/internal/awareness"
)

// EmbeddingBridge implements awareness.EmbeddingProvider by delegating to
// a NAP adapter's GenerateEmbeddings gRPC call. Thread-safe: the underlying
// client reference is swapped when the adapter reconfigures.
type EmbeddingBridge struct {
	mu     sync.RWMutex
	client napv1.IntentResolutionServiceClient
}

// NewEmbeddingBridge creates a new bridge with no active client.
func NewEmbeddingBridge() *EmbeddingBridge {
	return &EmbeddingBridge{}
}

// SetClient updates the gRPC client used for embedding generation.
// Pass nil to indicate no adapter is connected.
func (b *EmbeddingBridge) SetClient(client napv1.IntentResolutionServiceClient) {
	b.mu.Lock()
	b.client = client
	b.mu.Unlock()
}

// GenerateEmbeddings calls the NAP adapter's GenerateEmbeddings RPC.
// Returns an error if no client is connected or if the RPC fails.
func (b *EmbeddingBridge) GenerateEmbeddings(ctx context.Context, texts []string) (awareness.EmbeddingResult, error) {
	b.mu.RLock()
	c := b.client
	b.mu.RUnlock()

	if c == nil {
		return awareness.EmbeddingResult{}, fmt.Errorf("embedding: no AI adapter connected")
	}

	if len(texts) == 0 {
		return awareness.EmbeddingResult{}, nil
	}

	resp, err := c.GenerateEmbeddings(ctx, &napv1.EmbeddingRequest{Texts: texts})
	if err != nil {
		return awareness.EmbeddingResult{}, fmt.Errorf("embedding: gRPC error: %w", err)
	}
	if resp.ErrorMessage != "" {
		return awareness.EmbeddingResult{}, fmt.Errorf("embedding: provider error: %s", resp.ErrorMessage)
	}

	vectors := make([][]float32, len(resp.Embeddings))
	for i, ev := range resp.Embeddings {
		vectors[i] = ev.Values
	}
	return awareness.EmbeddingResult{
		Vectors:    vectors,
		Dimensions: int(resp.Dimensions),
		Model:      resp.ModelUsed,
	}, nil
}

// Compile-time check that EmbeddingBridge satisfies EmbeddingProvider.
var _ awareness.EmbeddingProvider = (*EmbeddingBridge)(nil)
