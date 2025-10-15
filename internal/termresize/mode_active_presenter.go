package termresize

// ActivePresenterMode designates the latest controller as the authoritative viewport.
type ActivePresenterMode struct{}

// NewActivePresenterMode instantiates the strategy placeholder.
func NewActivePresenterMode() Mode {
	return &ActivePresenterMode{}
}

func (m *ActivePresenterMode) Name() string {
	return "active_presenter"
}

func (m *ActivePresenterMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{"active_presenter mode pending implementation"},
	}, nil
}
