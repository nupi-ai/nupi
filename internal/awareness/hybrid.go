package awareness

import (
	"math"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// normalizeScores applies min-max normalization to SearchResult scores,
// mapping them to the [0, 1] range. If clampNegative is true, negative
// scores are clamped to 0 before normalization (used for cosine similarity).
func normalizeScores(results []SearchResult, clampNegative bool) []SearchResult {
	if len(results) == 0 {
		return results
	}

	if clampNegative {
		for i := range results {
			if results[i].Score < 0 {
				results[i].Score = 0
			}
		}
	}

	if len(results) == 1 {
		results[0].Score = 1.0
		return results
	}

	minScore := results[0].Score
	maxScore := results[0].Score
	for _, r := range results[1:] {
		if r.Score < minScore {
			minScore = r.Score
		}
		if r.Score > maxScore {
			maxScore = r.Score
		}
	}

	scoreRange := maxScore - minScore
	if scoreRange == 0 {
		for i := range results {
			results[i].Score = 1.0
		}
		return results
	}

	for i := range results {
		results[i].Score = (results[i].Score - minScore) / scoreRange
	}
	return results
}

// temporalDecay applies exponential decay to a score based on age.
// Formula: score × e^(-ln(2)/30 × age_days) with 30-day half-life.
// Returns score unchanged if mtime cannot be parsed.
func temporalDecay(score float64, mtime string, now time.Time) float64 {
	t, err := time.Parse(time.RFC3339Nano, mtime)
	if err != nil {
		// Try RFC3339 without nano.
		t, err = time.Parse(time.RFC3339, mtime)
		if err != nil {
			return score
		}
	}
	ageDays := now.Sub(t).Hours() / 24.0
	if ageDays <= 0 {
		return score
	}
	return score * math.Exp(-math.Ln2/30.0*ageDays)
}

// isEvergreenFile returns true if the file at path is a core memory file
// exempt from temporal decay. Checks the base filename (case-insensitive).
func isEvergreenFile(path string) bool {
	base := strings.ToLower(filepath.Base(path))
	switch base {
	case "soul.md", "identity.md", "user.md", "global.md", "project.md":
		return true
	}
	return false
}

// tokenize splits text into a set of lowercase tokens for Jaccard similarity.
// Splits on non-alphanumeric characters and discards tokens shorter than 2 chars.
func tokenize(text string) map[string]struct{} {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	tokens := make(map[string]struct{}, len(words))
	for _, w := range words {
		if utf8.RuneCountInString(w) >= 2 {
			tokens[w] = struct{}{}
		}
	}
	return tokens
}

// jaccardSimilarity computes |intersection| / |union| of two token sets.
// Returns 0 if both sets are empty.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}

	intersection := 0
	for k := range a {
		if _, ok := b[k]; ok {
			intersection++
		}
	}

	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// mmrRerank applies Maximal Marginal Relevance reranking for result diversity.
// λ controls the relevance-diversity trade-off (higher = more relevance).
// Uses Jaccard similarity on snippet tokens for diversity measurement.
func mmrRerank(results []SearchResult, lambda float64, maxResults int) []SearchResult {
	if len(results) == 0 || maxResults <= 0 {
		return nil
	}
	if len(results) == 1 {
		return results[:1]
	}
	if maxResults > len(results) {
		maxResults = len(results)
	}

	// Pre-tokenize all snippets.
	tokens := make([]map[string]struct{}, len(results))
	for i, r := range results {
		tokens[i] = tokenize(r.Snippet)
	}

	selected := make([]int, 0, maxResults)     // indices into results
	remaining := make(map[int]struct{}, len(results))
	for i := range results {
		remaining[i] = struct{}{}
	}

	// Select first: highest score.
	bestIdx := 0
	for i := range results {
		if results[i].Score > results[bestIdx].Score {
			bestIdx = i
		}
	}
	selected = append(selected, bestIdx)
	delete(remaining, bestIdx)

	// Greedily select remaining.
	for len(selected) < maxResults && len(remaining) > 0 {
		bestMMR := math.Inf(-1)
		bestCandidate := -1

		for idx := range remaining {
			relevance := lambda * results[idx].Score

			// Max similarity to any already-selected result.
			maxSim := 0.0
			for _, selIdx := range selected {
				sim := jaccardSimilarity(tokens[idx], tokens[selIdx])
				if sim > maxSim {
					maxSim = sim
				}
			}

			mmr := relevance - (1-lambda)*maxSim
			if mmr > bestMMR || bestCandidate == -1 {
				bestMMR = mmr
				bestCandidate = idx
			}
		}

		selected = append(selected, bestCandidate)
		delete(remaining, bestCandidate)
	}

	out := make([]SearchResult, len(selected))
	for i, idx := range selected {
		out[i] = results[idx]
	}
	return out
}
