package awareness

import "context"

// EmbeddingResult holds the output of an embedding generation call.
type EmbeddingResult struct {
	Vectors    [][]float32 // One vector per input text.
	Dimensions int         // Dimensionality of each vector.
	Model      string      // Model identifier used (e.g. "text-embedding-3-small").
}

// EmbeddingProvider generates vector embeddings for text inputs.
// Implementations must handle errors gracefully — embedding failures
// should never prevent FTS-only search from working (NFR33).
type EmbeddingProvider interface {
	// GenerateEmbeddings returns embedding vectors for the given texts.
	// A non-nil error means the provider is unavailable — caller should fall back to FTS-only.
	GenerateEmbeddings(ctx context.Context, texts []string) (EmbeddingResult, error)
}
