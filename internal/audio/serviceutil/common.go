package serviceutil

import (
	"context"
	"time"

	"github.com/nupi-ai/nupi/internal/audio/streammanager"
	"github.com/nupi-ai/nupi/internal/constants"
)

const (
	DefaultWorkerBuffer = 16
	DefaultRetryInitial = constants.Duration200Milliseconds
	DefaultRetryMax     = constants.Duration5Seconds
	DefaultMaxPending   = 100
)

type SessionParams = streammanager.SessionParams

type Factory[T any] interface {
	Create(ctx context.Context, params SessionParams) (T, error)
}

type FactoryFunc[T any] func(ctx context.Context, params SessionParams) (T, error)

func (f FactoryFunc[T]) Create(ctx context.Context, params SessionParams) (T, error) {
	return f(ctx, params)
}

func NormalizeRetryDelays(currentInitial, currentMax, initial, max time.Duration) (time.Duration, time.Duration) {
	if initial > 0 {
		currentInitial = initial
	}
	if max > 0 && max >= currentInitial {
		currentMax = max
	}
	if currentMax < currentInitial {
		currentMax = currentInitial
	}
	return currentInitial, currentMax
}
