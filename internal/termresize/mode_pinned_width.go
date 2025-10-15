package termresize

// PinnedWidthMode keeps width fixed while allowing height adjustments.
type PinnedWidthMode struct{}

// NewPinnedWidthMode creates the placeholder implementation for pinned width behaviour.
func NewPinnedWidthMode() Mode {
	return &PinnedWidthMode{}
}

func (m *PinnedWidthMode) Name() string {
	return "pinned_width"
}

func (m *PinnedWidthMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{"pinned_width mode pending implementation"},
	}, nil
}
