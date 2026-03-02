package server_test

import (
	"github.com/nupi-ai/nupi/internal/awareness"
	"github.com/nupi-ai/nupi/internal/server"
)

// Compile-time assertion: ConversationLogService must satisfy ConversationStore.
// Placed here (external test package) to avoid the import cycle
// awareness -> server -> awareness that would occur in awareness tests.
var _ server.ConversationStore = (*awareness.ConversationLogService)(nil)
