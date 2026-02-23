package constants

const (
	AdapterSlotSTT             = "stt"
	AdapterSlotTTS             = "tts"
	AdapterSlotAI              = "ai"
	AdapterSlotVAD             = "vad"
	AdapterSlotTunnel          = "tunnel"
	AdapterSlotToolHandler     = "tool-handler"
	AdapterSlotPipelineCleaner = "pipeline-cleaner"
)

// AllowedAdapterSlots lists adapter slots/types accepted by API/CLI validation.
var AllowedAdapterSlots = []string{
	AdapterSlotSTT,
	AdapterSlotTTS,
	AdapterSlotAI,
	AdapterSlotVAD,
	AdapterSlotTunnel,
	AdapterSlotToolHandler,
	AdapterSlotPipelineCleaner,
}

// RequiredAdapterSlots lists slots that must exist in seeded configuration.
var RequiredAdapterSlots = []string{
	AdapterSlotSTT,
	AdapterSlotAI,
	AdapterSlotTTS,
	AdapterSlotVAD,
	AdapterSlotTunnel,
}

const (
	AdapterTransportGRPC    = "grpc"
	AdapterTransportHTTP    = "http"
	AdapterTransportProcess = "process"

	DefaultAdapterTransport = AdapterTransportProcess
)

// AllowedAdapterTransports lists endpoint transports accepted by validation.
var AllowedAdapterTransports = []string{
	AdapterTransportProcess,
	AdapterTransportGRPC,
	AdapterTransportHTTP,
}
