package llmrunner_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// The transport failures the real backend and judge classify; the pipeline's
// own causes are covered over its fake backend.
func TestStart_FailureNamesItsCause(t *testing.T) {
	t.Parallel()

	providerDown := func(llm.Request) (llm.Response, error) {
		return llm.Response{}, errors.New("upstream 500: secret provider body")
	}
	overBudget := func(llm.Request) (llm.Response, error) {
		return llm.Response{Text: `{"impacted": false, "reason": "x"}`, Usage: llm.Usage{InputTokens: 100}}, nil
	}
	tests := []struct {
		name        string
		model       llm.Model
		badRemote   bool
		budget      int
		timeout     time.Duration
		want        review.FailureCause
		notCanceled bool
	}{
		{name: "provider error", model: &fakeModel{script: []func(llm.Request) (llm.Response, error){providerDown}}, want: review.CauseProvider},
		{name: "clone failure", model: &fakeModel{}, badRemote: true, want: review.CauseClone},
		{name: "token limit", model: &fakeModel{script: []func(llm.Request) (llm.Response, error){overBudget}}, budget: 10, want: review.CauseLimit},
		{name: "timeout", model: blockingModel{}, timeout: 200 * time.Millisecond, want: review.CauseTimeout, notCanceled: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repoDir, headSHA := newGitRepo(t)
			runner := newRunner(tc.model)
			runner.SetRemote(repoDir)
			if tc.badRemote {
				runner.SetRemote(filepath.Join(t.TempDir(), "missing.git"))
			}
			if tc.budget != 0 {
				runner.SetTokenBudget(tc.budget)
			}
			if tc.timeout != 0 {
				runner.SetTimeout(tc.timeout)
			}

			_, err := runner.Start(t.Context(), testRequest(headSHA))
			var failed *review.FailedError
			if !errors.As(err, &failed) || failed.Cause != tc.want {
				t.Fatalf("Start() = %v, want a *review.FailedError with cause %q", err, tc.want)
			}
			if tc.notCanceled && errors.Is(err, context.Canceled) {
				t.Errorf("Start() = %v, must not match context.Canceled", err)
			}
		})
	}
}
