package awareness

import (
	"strings"
)

// Chunk represents a fragment of a markdown file for FTS5 indexing.
type Chunk struct {
	Index   int    // Sequential index within the source file.
	Content string // Text content of the chunk.
}

const (
	maxChunkChars = 2000 // Sub-split threshold.
	minChunkChars = 20   // Merge threshold.
)

// ChunkMarkdown splits a markdown document into indexable chunks.
// Primary strategy: split on level-2 (##) and level-3 (###) headers.
// Fallback: split on paragraph boundaries (double newlines).
// Large chunks are sub-split; tiny chunks are merged with the previous one.
func ChunkMarkdown(content string) []Chunk {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	sections := splitByHeaders(content)
	if len(sections) <= 1 {
		// No headers found — fall back to paragraph splitting.
		sections = splitByParagraphs(content)
	}

	// Sub-split large sections on paragraph boundaries.
	var expanded []string
	for _, s := range sections {
		if len(s) > maxChunkChars {
			expanded = append(expanded, splitByParagraphs(s)...)
		} else {
			expanded = append(expanded, s)
		}
	}

	// Build chunks: skip empty, merge tiny fragments.
	var chunks []Chunk
	for _, text := range expanded {
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		if len(text) < minChunkChars && len(chunks) > 0 {
			// Merge into previous chunk.
			chunks[len(chunks)-1].Content += "\n\n" + text
			continue
		}
		chunks = append(chunks, Chunk{Index: len(chunks), Content: text})
	}

	return chunks
}

// splitByHeaders splits content on lines starting with "## " or "### ".
// Each header stays with its following body text.
// Headers inside fenced code blocks (``` or ~~~) are ignored.
func splitByHeaders(content string) []string {
	lines := strings.Split(content, "\n")
	var sections []string
	var current strings.Builder
	inCodeFence := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Track fenced code blocks so we don't split on headers inside them.
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inCodeFence = !inCodeFence
		}

		if !inCodeFence && (strings.HasPrefix(trimmed, "## ") || strings.HasPrefix(trimmed, "### ")) && current.Len() > 0 {
			sections = append(sections, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}

	if current.Len() > 0 {
		sections = append(sections, current.String())
	}

	return sections
}

// splitByParagraphs splits content on double newlines.
func splitByParagraphs(content string) []string {
	raw := strings.Split(content, "\n\n")
	var result []string
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
