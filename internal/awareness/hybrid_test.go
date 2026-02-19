package awareness

import (
	"math"
	"testing"
	"time"
)

// --- TestNormalizeScores (Task 6.1) ---

func TestNormalizeScoresEmpty(t *testing.T) {
	got := normalizeScores(nil, false)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestNormalizeScoresSingle(t *testing.T) {
	results := []SearchResult{{Score: 5.5}}
	got := normalizeScores(results, false)
	if got[0].Score != 1.0 {
		t.Fatalf("single result should be 1.0, got %f", got[0].Score)
	}
}

func TestNormalizeScoresMultiple(t *testing.T) {
	results := []SearchResult{
		{Score: 2.0},
		{Score: 6.0},
		{Score: 10.0},
	}
	got := normalizeScores(results, false)
	if got[0].Score != 0.0 {
		t.Fatalf("min score should be 0.0, got %f", got[0].Score)
	}
	if got[1].Score != 0.5 {
		t.Fatalf("mid score should be 0.5, got %f", got[1].Score)
	}
	if got[2].Score != 1.0 {
		t.Fatalf("max score should be 1.0, got %f", got[2].Score)
	}
}

func TestNormalizeScoresIdentical(t *testing.T) {
	results := []SearchResult{
		{Score: 3.0},
		{Score: 3.0},
		{Score: 3.0},
	}
	got := normalizeScores(results, false)
	for i, r := range got {
		if r.Score != 1.0 {
			t.Fatalf("identical scores should all be 1.0, got %f at index %d", r.Score, i)
		}
	}
}

func TestNormalizeScoresClampNegative(t *testing.T) {
	results := []SearchResult{
		{Score: -0.5},
		{Score: 0.3},
		{Score: 0.9},
	}
	got := normalizeScores(results, true)
	// After clamping: 0.0, 0.3, 0.9 → min=0.0, max=0.9
	if got[0].Score != 0.0 {
		t.Fatalf("clamped min should be 0.0, got %f", got[0].Score)
	}
	expected1 := 0.3 / 0.9
	if math.Abs(got[1].Score-expected1) > 1e-9 {
		t.Fatalf("expected %f, got %f", expected1, got[1].Score)
	}
	if got[2].Score != 1.0 {
		t.Fatalf("max should be 1.0, got %f", got[2].Score)
	}
}

func TestNormalizeScoresAllNegativeClamped(t *testing.T) {
	results := []SearchResult{
		{Score: -0.5},
		{Score: -0.3},
	}
	got := normalizeScores(results, true)
	// After clamping: both 0.0 → identical → all 1.0
	for i, r := range got {
		if r.Score != 1.0 {
			t.Fatalf("all-clamped should be 1.0, got %f at index %d", r.Score, i)
		}
	}
}

// --- TestTemporalDecay (Task 6.2) ---

func TestTemporalDecayRecentFile(t *testing.T) {
	now := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	mtime := now.Add(-24 * time.Hour).Format(time.RFC3339)
	got := temporalDecay(1.0, mtime, now)
	expected := math.Exp(-math.Ln2 / 30.0 * 1.0) // ~0.9772
	if math.Abs(got-expected) > 1e-4 {
		t.Fatalf("1-day decay: expected ~%f, got %f", expected, got)
	}
}

func TestTemporalDecay30Days(t *testing.T) {
	now := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	mtime := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	got := temporalDecay(1.0, mtime, now)
	if math.Abs(got-0.5) > 1e-4 {
		t.Fatalf("30-day decay should be ~0.5, got %f", got)
	}
}

func TestTemporalDecay90Days(t *testing.T) {
	now := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	mtime := now.Add(-90 * 24 * time.Hour).Format(time.RFC3339)
	got := temporalDecay(1.0, mtime, now)
	if math.Abs(got-0.125) > 1e-4 {
		t.Fatalf("90-day decay should be ~0.125, got %f", got)
	}
}

func TestTemporalDecayParseError(t *testing.T) {
	now := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	got := temporalDecay(0.8, "not-a-date", now)
	if got != 0.8 {
		t.Fatalf("parse error should return score unchanged, got %f", got)
	}
}

func TestTemporalDecayZeroAge(t *testing.T) {
	now := time.Date(2026, 2, 19, 12, 0, 0, 0, time.UTC)
	mtime := now.Format(time.RFC3339)
	got := temporalDecay(0.9, mtime, now)
	if got != 0.9 {
		t.Fatalf("zero age should return score unchanged, got %f", got)
	}
}

// --- TestIsEvergreenFile (Task 6.3) ---

func TestIsEvergreenFilePROJECT(t *testing.T) {
	if !isEvergreenFile("projects/myproject/PROJECT.md") {
		t.Fatal("PROJECT.md should be evergreen")
	}
}

func TestIsEvergreenFileSOUL(t *testing.T) {
	if !isEvergreenFile("SOUL.md") {
		t.Fatal("SOUL.md should be evergreen")
	}
}

func TestIsEvergreenFileRegular(t *testing.T) {
	if isEvergreenFile("daily/2026-02-19.md") {
		t.Fatal("daily file should not be evergreen")
	}
}

func TestIsEvergreenFileCaseInsensitive(t *testing.T) {
	if !isEvergreenFile("projects/foo/project.md") {
		t.Fatal("project.md (lowercase) should be evergreen")
	}
}

func TestIsEvergreenFileNestedPath(t *testing.T) {
	if !isEvergreenFile("projects/nupi/PROJECT.md") {
		t.Fatal("nested PROJECT.md should be evergreen")
	}
	if isEvergreenFile("projects/nupi/topics/architecture.md") {
		t.Fatal("nested regular file should not be evergreen")
	}
}

// --- TestTokenize (Task 6.4) ---

func TestTokenizeBasicSentence(t *testing.T) {
	tokens := tokenize("hello world test")
	if _, ok := tokens["hello"]; !ok {
		t.Fatal("expected 'hello' in tokens")
	}
	if _, ok := tokens["world"]; !ok {
		t.Fatal("expected 'world' in tokens")
	}
	if _, ok := tokens["test"]; !ok {
		t.Fatal("expected 'test' in tokens")
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens, got %d", len(tokens))
	}
}

func TestTokenizePunctuation(t *testing.T) {
	tokens := tokenize("database-migration! (schema v2)")
	if _, ok := tokens["database"]; !ok {
		t.Fatal("expected 'database' in tokens")
	}
	if _, ok := tokens["migration"]; !ok {
		t.Fatal("expected 'migration' in tokens")
	}
	if _, ok := tokens["schema"]; !ok {
		t.Fatal("expected 'schema' in tokens")
	}
	if _, ok := tokens["v2"]; !ok {
		t.Fatal("expected 'v2' in tokens")
	}
}

func TestTokenizeShortTokensFiltered(t *testing.T) {
	tokens := tokenize("I am a go dev")
	// "i", "a" should be filtered (< 2 chars)
	if _, ok := tokens["i"]; ok {
		t.Fatal("single-char 'i' should be filtered")
	}
	if _, ok := tokens["a"]; ok {
		t.Fatal("single-char 'a' should be filtered")
	}
	if _, ok := tokens["am"]; !ok {
		t.Fatal("expected 'am' in tokens")
	}
	if _, ok := tokens["go"]; !ok {
		t.Fatal("expected 'go' in tokens")
	}
	if _, ok := tokens["dev"]; !ok {
		t.Fatal("expected 'dev' in tokens")
	}
}

func TestTokenizeEmpty(t *testing.T) {
	tokens := tokenize("")
	if len(tokens) != 0 {
		t.Fatalf("empty string should yield 0 tokens, got %d", len(tokens))
	}
}

func TestTokenizeUnicode(t *testing.T) {
	tokens := tokenize("baza danych migracja")
	if _, ok := tokens["baza"]; !ok {
		t.Fatal("expected 'baza' in tokens")
	}
	if _, ok := tokens["danych"]; !ok {
		t.Fatal("expected 'danych' in tokens")
	}
	if _, ok := tokens["migracja"]; !ok {
		t.Fatal("expected 'migracja' in tokens")
	}
}

func TestTokenizeSingleRuneMultiByte(t *testing.T) {
	// Single multi-byte runes (Polish ą=2bytes, Chinese 中=3bytes) should be filtered
	// because they are 1 character, even though len() > 1.
	tokens := tokenize("ą ę 中")
	if _, ok := tokens["ą"]; ok {
		t.Fatal("single-rune 'ą' should be filtered (1 character)")
	}
	if _, ok := tokens["ę"]; ok {
		t.Fatal("single-rune 'ę' should be filtered (1 character)")
	}
	if _, ok := tokens["中"]; ok {
		t.Fatal("single-rune '中' should be filtered (1 character)")
	}
	if len(tokens) != 0 {
		t.Fatalf("expected 0 tokens from single-rune chars, got %d", len(tokens))
	}
}

// --- TestJaccardSimilarity (Task 6.5) ---

func TestJaccardIdentical(t *testing.T) {
	a := map[string]struct{}{"hello": {}, "world": {}}
	got := jaccardSimilarity(a, a)
	if got != 1.0 {
		t.Fatalf("identical sets should be 1.0, got %f", got)
	}
}

func TestJaccardDisjoint(t *testing.T) {
	a := map[string]struct{}{"hello": {}}
	b := map[string]struct{}{"world": {}}
	got := jaccardSimilarity(a, b)
	if got != 0.0 {
		t.Fatalf("disjoint sets should be 0.0, got %f", got)
	}
}

func TestJaccardPartialOverlap(t *testing.T) {
	a := map[string]struct{}{"hello": {}, "world": {}, "test": {}}
	b := map[string]struct{}{"hello": {}, "world": {}, "foo": {}}
	// intersection=2, union=4
	got := jaccardSimilarity(a, b)
	if got != 0.5 {
		t.Fatalf("expected 0.5, got %f", got)
	}
}

func TestJaccardBothEmpty(t *testing.T) {
	a := map[string]struct{}{}
	b := map[string]struct{}{}
	got := jaccardSimilarity(a, b)
	if got != 0.0 {
		t.Fatalf("empty sets should be 0.0, got %f", got)
	}
}

func TestJaccardOneEmpty(t *testing.T) {
	a := map[string]struct{}{"hello": {}}
	b := map[string]struct{}{}
	got := jaccardSimilarity(a, b)
	if got != 0.0 {
		t.Fatalf("one empty should be 0.0, got %f", got)
	}
}

// --- TestMMRRerank (Task 6.6) ---

func TestMMRRerankDiverse(t *testing.T) {
	// Three results: two very similar snippets (high Jaccard), one different.
	// MMR should prefer the different one over the similar redundant one.
	results := []SearchResult{
		{Score: 0.9, Snippet: "database migration schema update"},
		{Score: 0.85, Snippet: "database migration schema change"},  // similar to [0]
		{Score: 0.7, Snippet: "authentication login flow security"}, // different
	}
	got := mmrRerank(results, 0.7, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	// First should be highest score.
	if got[0].Score != 0.9 {
		t.Fatalf("first result should be highest score 0.9, got %f", got[0].Score)
	}
	// Second should prefer the diverse result over the similar one.
	if got[1].Snippet != "authentication login flow security" {
		t.Fatalf("MMR should prefer diverse result, got: %s", got[1].Snippet)
	}
}

func TestMMRRerankRespectsMaxResults(t *testing.T) {
	results := []SearchResult{
		{Score: 0.9, Snippet: "one"},
		{Score: 0.8, Snippet: "two"},
		{Score: 0.7, Snippet: "three"},
		{Score: 0.6, Snippet: "four"},
	}
	got := mmrRerank(results, 0.7, 2)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
}

func TestMMRRerankSingleResult(t *testing.T) {
	results := []SearchResult{{Score: 0.5, Snippet: "only one"}}
	got := mmrRerank(results, 0.7, 5)
	if len(got) != 1 {
		t.Fatalf("expected 1 result, got %d", len(got))
	}
	if got[0].Score != 0.5 {
		t.Fatalf("expected score 0.5, got %f", got[0].Score)
	}
}

func TestMMRRerankPureRelevance(t *testing.T) {
	// λ=1.0 means pure relevance, no diversity. Should return in score order.
	results := []SearchResult{
		{Score: 0.5, Snippet: "low score"},
		{Score: 0.9, Snippet: "high score"},
		{Score: 0.7, Snippet: "mid score"},
	}
	got := mmrRerank(results, 1.0, 3)
	if got[0].Score != 0.9 {
		t.Fatalf("expected first score 0.9, got %f", got[0].Score)
	}
	if got[1].Score != 0.7 {
		t.Fatalf("expected second score 0.7, got %f", got[1].Score)
	}
	if got[2].Score != 0.5 {
		t.Fatalf("expected third score 0.5, got %f", got[2].Score)
	}
}

func TestMMRRerankPureDiversity(t *testing.T) {
	// λ=0.0 means pure diversity. Should pick the most different result second.
	results := []SearchResult{
		{Score: 0.9, Snippet: "database migration schema update"},
		{Score: 0.85, Snippet: "database migration schema change"},  // similar to [0]
		{Score: 0.1, Snippet: "authentication login flow security"}, // very different
	}
	got := mmrRerank(results, 0.0, 2)
	// First is highest score (initial pick).
	if got[0].Score != 0.9 {
		t.Fatalf("first should be highest score, got %f", got[0].Score)
	}
	// Second should be the most diverse (lowest Jaccard with first), ignoring score.
	if got[1].Snippet != "authentication login flow security" {
		t.Fatalf("pure diversity should pick most different result, got: %s", got[1].Snippet)
	}
}
