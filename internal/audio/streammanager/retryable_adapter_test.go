package streammanager

import (
	"context"
	"errors"
	"testing"
	"time"
)

type testAdapter struct {
	id int
}

func TestRetryableAdapterRecoversOnUnavailable(t *testing.T) {
	errUnavailable := errors.New("adapter unavailable")

	createCalls := 0
	closeCalls := 0
	processed := []int{}
	published := []int{}

	ra := NewRetryableAdapter(RetryableAdapterConfig[*testAdapter, int]{
		Create: func(context.Context) (*testAdapter, error) {
			createCalls++
			return &testAdapter{id: createCalls + 1}, nil
		},
		Close: func(context.Context, *testAdapter) ([]int, error) {
			closeCalls++
			return nil, nil
		},
		IsAdapterUnavailable: func(err error) bool {
			return errors.Is(err, errUnavailable)
		},
		RecoveryCloseTimeout: 50 * time.Millisecond,
	})
	ra.SetActive(&testAdapter{id: 1})

	// First call fails as adapter-unavailable and should trigger recovery close.
	ra.Process(context.Background(), func(_ context.Context, a *testAdapter) ([]int, error) {
		processed = append(processed, a.id)
		return []int{11}, errUnavailable
	}, func(out int) {
		published = append(published, out)
	})

	// Second call should create a new adapter.
	ra.Process(context.Background(), func(_ context.Context, a *testAdapter) ([]int, error) {
		processed = append(processed, a.id)
		return []int{22}, nil
	}, func(out int) {
		published = append(published, out)
	})

	if closeCalls != 1 {
		t.Fatalf("expected 1 recovery close, got %d", closeCalls)
	}
	if createCalls != 1 {
		t.Fatalf("expected 1 create call after recovery, got %d", createCalls)
	}
	if len(processed) != 2 || processed[0] != 1 || processed[1] != 2 {
		t.Fatalf("unexpected processed adapters: %+v", processed)
	}
	if len(published) != 2 || published[0] != 11 || published[1] != 22 {
		t.Fatalf("unexpected published outputs: %+v", published)
	}
}

func TestRetryableAdapterClosePublishesOutputs(t *testing.T) {
	closeCalls := 0
	published := []int{}

	ra := NewRetryableAdapter(RetryableAdapterConfig[*testAdapter, int]{
		Close: func(context.Context, *testAdapter) ([]int, error) {
			closeCalls++
			return []int{7, 8}, nil
		},
	})
	ra.SetActive(&testAdapter{id: 1})

	ra.Close("shutdown", 50*time.Millisecond, func(out int) {
		published = append(published, out)
	})

	if closeCalls != 1 {
		t.Fatalf("expected close call, got %d", closeCalls)
	}
	if len(published) != 2 || published[0] != 7 || published[1] != 8 {
		t.Fatalf("unexpected close outputs: %+v", published)
	}
}
