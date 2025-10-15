package termresize

// SnapshotRelayMode maintains buffered snapshots to initialise new viewers.
type SnapshotRelayMode struct{}

// NewSnapshotRelayMode creates the placeholder implementation for snapshot relay behaviour.
func NewSnapshotRelayMode() Mode {
	return &SnapshotRelayMode{}
}

func (m *SnapshotRelayMode) Name() string {
	return "snapshot_relay"
}

func (m *SnapshotRelayMode) Handle(ctx *Context) (ResizeDecision, error) {
	return ResizeDecision{
		SessionID: ctx.Request.SessionID,
		Notes:     []string{"snapshot_relay mode pending implementation"},
	}, nil
}
