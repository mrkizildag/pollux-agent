package review_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestOneLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, in string
		limit    int
		want     string
	}{
		{"collapses whitespace", "a\n\tb  c", 10, "a b c"},
		{"cuts at the limit", "abcdef", 3, "abc..."},
		{"drops a split rune", "aé", 2, "a..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := review.OneLine(tt.in, tt.limit); got != tt.want {
				t.Errorf("OneLine(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
			}
		})
	}
}
