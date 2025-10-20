package termresize

// NewManagerWithDefaults registers the production-ready host-lock mode so the PTY size is
// always anchored to the host terminal. Remaining modes stay unregistered until their
// implementations are complete.
func NewManagerWithDefaults() (*Manager, error) {
	hostLock := NewHostLockMode()

	manager, err := NewManager(hostLock)
	if err != nil {
		return nil, err
	}

	// Host lock is the default behaviour: host terminal dictates PTY sizing.
	if err := manager.SetDefaultMode(hostLock.Name()); err != nil {
		return nil, err
	}

	return manager, nil
}
