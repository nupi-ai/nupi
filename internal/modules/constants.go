package modules

// Built-in adapter identifiers used by quickstart and tests.
const (
	MockSTTAdapterID = "adapter.stt.mock"
	MockTTSAdapterID = "adapter.tts.mock"
	MockVADAdapterID = "adapter.vad.mock"
)

// RequiredReferenceAdapters lists built-in adapters expected to be available for quickstart completion.
var RequiredReferenceAdapters = []string{
	MockSTTAdapterID,
	MockTTSAdapterID,
}
