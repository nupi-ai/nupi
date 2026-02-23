package constants

const (
	NotificationEventTaskCompleted = "TASK_COMPLETED"
	NotificationEventInputNeeded   = "INPUT_NEEDED"
	NotificationEventError         = "ERROR"
)

const (
	PipelineWaitingForUserInput    = "user_input"
	PipelineWaitingForConfirmation = "confirmation"
	PipelineWaitingForChoice       = "choice"
)

const (
	SpeakMetadataTypeKey   = "type"
	SpeakTypeClarification = "clarification"
	SpeakTypeError         = "error"
	SpeakTypeCompletion    = "completion"
)
