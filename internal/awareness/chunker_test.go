package awareness

import (
	"strings"
	"testing"
)

func TestChunkMarkdownEmpty(t *testing.T) {
	chunks := ChunkMarkdown("")
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for empty input, got %d", len(chunks))
	}

	chunks = ChunkMarkdown("   \n\n  ")
	if len(chunks) != 0 {
		t.Fatalf("expected 0 chunks for whitespace-only input, got %d", len(chunks))
	}
}

func TestChunkMarkdownSplitByHeaders(t *testing.T) {
	content := `# Title

Some intro text.

## Section One

Content of section one.

## Section Two

Content of section two.

### Subsection

More content here.`

	chunks := ChunkMarkdown(content)
	if len(chunks) < 3 {
		t.Fatalf("expected at least 3 chunks from header-split content, got %d", len(chunks))
	}

	// First chunk should contain the intro.
	if !strings.Contains(chunks[0].Content, "Some intro text") {
		t.Errorf("first chunk should contain intro text, got: %q", chunks[0].Content)
	}

	// Check that headers are included in their chunks.
	found := false
	for _, ch := range chunks {
		if strings.Contains(ch.Content, "## Section Two") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a chunk containing '## Section Two' header")
	}

	// Verify sequential indices.
	for i, ch := range chunks {
		if ch.Index != i {
			t.Errorf("chunk %d has Index=%d, expected %d", i, ch.Index, i)
		}
	}
}

func TestChunkMarkdownFallbackToParagraphs(t *testing.T) {
	content := `First paragraph with some text.

Second paragraph with more text.

Third paragraph ends here.`

	chunks := ChunkMarkdown(content)
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks from paragraph-split, got %d", len(chunks))
	}

	if !strings.Contains(chunks[0].Content, "First paragraph") {
		t.Errorf("first chunk should contain 'First paragraph', got: %q", chunks[0].Content)
	}
	if !strings.Contains(chunks[2].Content, "Third paragraph") {
		t.Errorf("third chunk should contain 'Third paragraph', got: %q", chunks[2].Content)
	}
}

func TestChunkMarkdownLargeChunkSubSplit(t *testing.T) {
	// Create a single section larger than maxChunkChars with no sub-headers.
	para := strings.Repeat("word ", 100) // ~500 chars per paragraph
	content := "## Big Section\n\n"
	for i := 0; i < 6; i++ {
		content += para + "\n\n"
	}

	chunks := ChunkMarkdown(content)
	// With ~3000 chars total and maxChunkChars=2000, it should be sub-split.
	if len(chunks) < 2 {
		t.Fatalf("expected large chunk to be sub-split, got %d chunk(s)", len(chunks))
	}

	for _, ch := range chunks {
		if len(ch.Content) > maxChunkChars+200 { // Allow some margin for merging.
			t.Errorf("chunk exceeds max size: %d chars", len(ch.Content))
		}
	}
}

func TestChunkMarkdownMergeSmallChunks(t *testing.T) {
	content := `## A

Substantial content here with enough text to pass minimum.

## B

ok

## C

Another section with proper content here.`

	chunks := ChunkMarkdown(content)
	// "ok" is less than minChunkChars (20) and should be merged with previous.
	for _, ch := range chunks {
		if ch.Content == "ok" {
			t.Error("tiny chunk 'ok' should have been merged with previous chunk")
		}
	}
}

func TestChunkMarkdownSingleContent(t *testing.T) {
	content := "Just a single paragraph of text."
	chunks := ChunkMarkdown(content)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk for single paragraph, got %d", len(chunks))
	}
	if chunks[0].Content != content {
		t.Errorf("expected chunk content to match input, got: %q", chunks[0].Content)
	}
	if chunks[0].Index != 0 {
		t.Errorf("expected Index=0, got %d", chunks[0].Index)
	}
}

func TestChunkMarkdownMixedHeadersAndParagraphs(t *testing.T) {
	content := `## Daily Log

Morning: reviewed PRs.

Afternoon: implemented feature X.

## Notes

Remember to update docs.`

	chunks := ChunkMarkdown(content)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}

	// First chunk should contain daily log content.
	if !strings.Contains(chunks[0].Content, "Morning") {
		t.Error("first chunk should contain 'Morning'")
	}

	// Last chunk should contain notes.
	last := chunks[len(chunks)-1]
	if !strings.Contains(last.Content, "Remember to update docs") {
		t.Error("last chunk should contain 'Remember to update docs'")
	}
}

func TestChunkMarkdownCodeFenceHeaders(t *testing.T) {
	content := "## Real Header\n\nSome text before code.\n\n```markdown\n## Not A Header\n\nThis is inside a code block.\n```\n\nText after code block.\n\n## Another Real Header\n\nFinal content."

	chunks := ChunkMarkdown(content)

	// Should split on the two real headers, NOT on the one inside the code fence.
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks (2 real headers), got %d", len(chunks))
	}

	// First chunk should contain both the intro and the code block (no split inside fence).
	if !strings.Contains(chunks[0].Content, "## Not A Header") {
		t.Error("first chunk should contain the fenced header as content, not split on it")
	}
	if !strings.Contains(chunks[0].Content, "Text after code block") {
		t.Error("first chunk should contain text after the code block")
	}

	// Second chunk should be the real header after the code block.
	if !strings.Contains(chunks[1].Content, "## Another Real Header") {
		t.Error("second chunk should contain '## Another Real Header'")
	}
}
