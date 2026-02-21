package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "direct NotFoundError",
			err:  NotFoundError{Entity: "test", Key: "k"},
			want: true,
		},
		{
			name: "wrapped NotFoundError",
			err:  fmt.Errorf("outer: %w", NotFoundError{Entity: "test"}),
			want: true,
		},
		{
			name: "double-wrapped NotFoundError",
			err:  fmt.Errorf("a: %w", fmt.Errorf("b: %w", NotFoundError{})),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "other error type",
			err:  errors.New("something"),
			want: false,
		},
		{
			name: "wrapped other error",
			err:  fmt.Errorf("wrap: %w", errors.New("x")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsNotFound(tt.err); got != tt.want {
				t.Errorf("IsNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNotFoundErrorMessage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  NotFoundError
		want string
	}{
		{
			name: "with key",
			err:  NotFoundError{Entity: "test", Key: "k"},
			want: "test k not found",
		},
		{
			name: "without key",
			err:  NotFoundError{Entity: "test"},
			want: "test not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.err.Error(); got != tt.want {
				t.Errorf("NotFoundError.Error() = %q, want %q", got, tt.want)
			}
		})
	}
}
