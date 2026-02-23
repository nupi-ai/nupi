package termresize

// NewStubMode creates a placeholder strategy for modes pending implementation.
func NewStubMode(name string) Mode {
	return &pendingMode{name: name}
}

type pendingMode struct {
	name string
}

func (m *pendingMode) Name() string {
	return m.name
}

func (m *pendingMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{m.name + " mode pending implementation"},
	}, nil
}
