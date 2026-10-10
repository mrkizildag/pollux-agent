package pipeline

import (
	"fmt"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Meter is one run's token count, charged by the Judge and the backend alike.
type Meter struct {
	max   int
	total review.Tokens
}

// NewMeter returns a Meter that allows up to maxTokens total input and output tokens.
func NewMeter(maxTokens int) *Meter {
	return &Meter{max: maxTokens}
}

// Charge adds t to the running total and returns an error wrapping ErrLimit
// once input plus output exceeds the cap.
func (m *Meter) Charge(t review.Tokens) error {
	m.total.Input += t.Input
	m.total.Output += t.Output
	m.total.CacheRead += t.CacheRead
	m.total.CacheWrite += t.CacheWrite
	if used := m.total.Input + m.total.Output; used > int64(m.max) {
		return fmt.Errorf("used %d tokens, budget %d: %w", used, m.max, ErrLimit)
	}
	return nil
}

// Usage is what was charged so far; nil when nothing was. A nil Meter, for a
// run that called no model, reports nil.
func (m *Meter) Usage() *review.Usage {
	if m == nil || m.total == (review.Tokens{}) {
		return nil
	}
	t := m.total
	return &review.Usage{Tokens: &t}
}
