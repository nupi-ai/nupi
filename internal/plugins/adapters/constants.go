package adapters

// Built-in adapter identifiers used by quickstart and tests.
const (
	MockSTTAdapterID = "adapter.stt.mock"
	MockTTSAdapterID = "adapter.tts.mock"
	MockVADAdapterID = "adapter.vad.mock"
	MockAIAdapterID  = "adapter.ai.mock"
)

// Runtime metadata keys propagated with adapter status events.
const (
	RuntimeExtraTransport = "transport"
	RuntimeExtraAddress   = "address"
)

// RequiredReferenceAdapters lists built-in adapters expected to be available for quickstart completion.
var RequiredReferenceAdapters = []string{
	MockSTTAdapterID,
	MockTTSAdapterID,
}
