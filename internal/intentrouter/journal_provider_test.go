package intentrouter_test

import (
	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/intentrouter"
)

// Compile-time assertion: JournalService must satisfy JournalProvider.
// Placed here (external test package) to avoid the import cycle
// awareness -> intentrouter -> awareness that would occur in awareness tests.
var _ intentrouter.JournalProvider = (*awareness.JournalService)(nil)
