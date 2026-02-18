package language

import "context"

// Metadata key constants used in NAP adapter request metadata maps.
const (
	MetaISO1    = "nupi.lang.iso1"
	MetaBCP47   = "nupi.lang.bcp47"
	MetaEnglish = "nupi.lang.english"
	MetaNative  = "nupi.lang.native"
)

// GRPCHeader is the gRPC metadata key clients use to send their language preference.
const GRPCHeader = "nupi-language"

type langContextKey struct{}

// WithLanguage returns a new context carrying the given Language.
func WithLanguage(ctx context.Context, lang Language) context.Context {
	return context.WithValue(ctx, langContextKey{}, lang)
}

// FromContext extracts the Language stored by WithLanguage.
// The second return value is false if no language was stored.
func FromContext(ctx context.Context) (Language, bool) {
	if ctx == nil {
		return Language{}, false
	}
	lang, ok := ctx.Value(langContextKey{}).(Language)
	return lang, ok
}

// MergeToMetadata adds the four nupi.lang.* keys to the given metadata map.
// If m is nil, a new map is created and returned.
func MergeToMetadata(lang Language, m map[string]string) map[string]string {
	if m == nil {
		m = make(map[string]string, 4)
	}
	m[MetaISO1] = lang.ISO1
	m[MetaBCP47] = lang.BCP47
	m[MetaEnglish] = lang.EnglishName
	m[MetaNative] = lang.NativeName
	return m
}

// MergeContextLanguage is a convenience that reads the language from ctx
// and merges it into the metadata map. If no language is in the context,
// the map is returned unchanged.
func MergeContextLanguage(ctx context.Context, m map[string]string) map[string]string {
	lang, ok := FromContext(ctx)
	if !ok {
		return m
	}
	return MergeToMetadata(lang, m)
}

// MetadataKeys returns the four standard language metadata key constants
// (nupi.lang.iso1, nupi.lang.bcp47, nupi.lang.english, nupi.lang.native).
// Useful for whitelist checks or metadata validation by external consumers.
func MetadataKeys() []string {
	return []string{MetaISO1, MetaBCP47, MetaEnglish, MetaNative}
}
