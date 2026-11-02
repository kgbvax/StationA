package logging

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestCleanStop(t *testing.T) {
	done, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	wrapped := fmt.Errorf("mqtt connect: %w", context.Canceled)

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil error", done, nil, false},
		{"shutdown, bare canceled", done, context.Canceled, true},
		{"shutdown, wrapped canceled", done, wrapped, true},
		{"shutdown, real failure", done, errors.New("boom"), false},
		{"no shutdown, canceled", live, context.Canceled, false},
		{"no shutdown, real failure", live, errors.New("boom"), false},
	}
	for _, c := range cases {
		if got := CleanStop(c.ctx, c.err); got != c.want {
			t.Errorf("%s: CleanStop = %v, want %v", c.name, got, c.want)
		}
	}
}
