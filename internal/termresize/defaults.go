package termresize

// NewManagerWithDefaults registers placeholder modes and returns a manager instance.
func NewManagerWithDefaults() (*Manager, error) {
	noop := NewNoopMode()
	hostLock := NewHostLockMode()
	presenter := NewActivePresenterMode()
	guided := NewGuidedFitMode()
	pinned := NewPinnedWidthMode()
	snapshot := NewSnapshotRelayMode()

	manager, err := NewManager(noop, hostLock, presenter, guided, pinned, snapshot)
	if err != nil {
		return nil, err
	}

	// Host lock is the default behaviour: host terminal dictates PTY sizing.
	if err := manager.SetDefaultMode(hostLock.Name()); err != nil {
		return nil, err
	}

	return manager, nil
}
