package intentrouter_test

import (
	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/intentrouter"
)

// Compile-time assertion: ConversationLogService must satisfy ConversationLogProvider.
// Placed here (external test package) to avoid the import cycle
// awareness -> intentrouter -> awareness that would occur in awareness tests.
var _ intentrouter.ConversationLogProvider = (*awareness.ConversationLogService)(nil)
