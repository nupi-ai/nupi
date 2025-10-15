package termresize

// GuidedFitMode spawns independent PTY geometry per participant.
type GuidedFitMode struct{}

// NewGuidedFitMode creates a placeholder guided-fit strategy.
func NewGuidedFitMode() Mode {
	return &GuidedFitMode{}
}

func (m *GuidedFitMode) Name() string {
	return "guided_fit"
}

func (m *GuidedFitMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{"guided_fit mode pending implementation"},
	}, nil
}
