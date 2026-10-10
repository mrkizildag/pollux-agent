package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestStart_FailureNamesItsCause(t *testing.T) {
	t.Parallel()

	overBudget := triage(false).withTokens(review.Tokens{Input: 100})
	tests := []struct {
		name    string
		judge   *fakeJudge
		backend *fakeBackend
		limits  pipeline.Limits
		baseErr error
		want    review.FailureCause
		wantIs  error
	}{
		{
			name:    "judge provider error",
			judge:   newJudge().on(kindTriage, reply{err: fmt.Errorf("complete: %w", pipeline.ErrProvider)}),
			backend: &fakeBackend{},
			want:    review.CauseProvider,
			wantIs:  pipeline.ErrProvider,
		},
		{
			name:    "backend provider error",
			judge:   newJudge().on(kindTriage, triage(true)),
			backend: &fakeBackend{runErr: fmt.Errorf("run agent: %w", pipeline.ErrProvider)},
			want:    review.CauseProvider,
			wantIs:  pipeline.ErrProvider,
		},
		{
			name:    "clone failure",
			judge:   newJudge(),
			backend: &fakeBackend{openErr: fmt.Errorf("clone: %w", pipeline.ErrWorkspace)},
			want:    review.CauseClone,
			wantIs:  pipeline.ErrWorkspace,
		},
		{
			name:    "base docs unreadable",
			baseErr: errors.New("git show failed"),
			judge:   newJudge(),
			backend: &fakeBackend{},
			want:    review.CauseClone,
			wantIs:  pipeline.ErrWorkspace,
		},
		{
			name:    "token limit in the judge",
			judge:   newJudge().on(kindTriage, overBudget),
			backend: &fakeBackend{},
			limits:  pipeline.Limits{Steps: 12, Tokens: 10, Deadline: time.Minute},
			want:    review.CauseLimit,
			wantIs:  pipeline.ErrLimit,
		},
		{
			name:    "token limit in the backend",
			judge:   newJudge().on(kindTriage, triage(true)),
			backend: &fakeBackend{charge: review.Tokens{Input: 100}},
			limits:  pipeline.Limits{Steps: 12, Tokens: 10, Deadline: time.Minute},
			want:    review.CauseLimit,
			wantIs:  pipeline.ErrLimit,
		},
		{
			name:    "step limit",
			judge:   newJudge().on(kindTriage, triage(true)),
			backend: &fakeBackend{runErr: fmt.Errorf("run agent: %w", pipeline.ErrLimit)},
			want:    review.CauseLimit,
			wantIs:  pipeline.ErrLimit,
		},
		{
			name:    "timeout in the backend",
			judge:   newJudge().on(kindTriage, triage(true)),
			backend: &fakeBackend{runErr: fmt.Errorf("run agent: %w", pipeline.ErrTimeout)},
			want:    review.CauseTimeout,
			wantIs:  pipeline.ErrTimeout,
		},
		{
			name:    "a context deadline in the judge",
			judge:   newJudge().on(kindTriage, reply{err: fmt.Errorf("complete: %w", context.DeadlineExceeded)}),
			backend: &fakeBackend{},
			want:    review.CauseTimeout,
			wantIs:  pipeline.ErrTimeout,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ws := xWorkspace()
			ws.baseErr = tc.baseErr
			limits := tc.limits
			if limits == (pipeline.Limits{}) {
				limits = pipeline.ReviewLimits()
			}
			a := analyzeWith(t, limits, ws, tc.judge, tc.backend)
			if got := a.failure(t).Cause; got != tc.want {
				t.Fatalf("Start() = %v, want a *review.FailedError with cause %q, got %q", a.err, tc.want, got)
			}
			if !errors.Is(a.err, tc.wantIs) {
				t.Errorf("Start() = %v, want errors.Is %v", a.err, tc.wantIs)
			}
		})
	}
}

func TestStart_TimeoutIsNotACancellation(t *testing.T) {
	t.Parallel()

	a := analyze(t, xWorkspace(), newJudge().on(kindTriage, reply{err: fmt.Errorf("complete: %w", context.DeadlineExceeded)}), &fakeBackend{})
	if errors.Is(a.err, context.Canceled) {
		t.Errorf("Start() = %v, must not match context.Canceled", a.err)
	}
}
