package termresize

import "fmt"

// Mode encapsulates a strategy for reconciling viewport changes across participants.
type Mode interface {
	Name() string
	Handle(ctx *Context) (ResizeDecision, error)
}

// ErrUnknownMode is returned when a requested mode is not registered.
var ErrUnknownMode = fmt.Errorf("unknown terminal resize mode")
