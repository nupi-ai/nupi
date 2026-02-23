package mapper

import "github.com/nupi-ai/nupi/internal/sanitize"

type MetadataLimits = sanitize.MetadataLimits

func NormalizeAndValidateMetadata(raw map[string]string, limits MetadataLimits) (map[string]string, error) {
	return sanitize.NormalizeAndValidateMetadata(raw, limits)
}
