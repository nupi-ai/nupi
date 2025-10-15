package termresize

// noopMode is the default placeholder mode used until a concrete strategy is configured.
type noopMode struct{}

// NewNoopMode returns a mode that produces no PTY or broadcast changes.
func NewNoopMode() Mode {
	return &noopMode{}
}

func (n *noopMode) Name() string {
	return "noop"
}

func (n *noopMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{"noop mode: resize ignored"},
	}, nil
}
