package llmrunner_test

import (
	"errors"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// A model that never submits runs the agent loop into its step limit.
func TestStart_StepLimitEndsTheDraftAsLimit(t *testing.T) {
	t.Parallel()

	script := []func(llm.Request) (llm.Response, error){triageResponse(true)}
	for range 3 {
		script = append(script, func(llm.Request) (llm.Response, error) {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: []byte(`{"path":"docs/x.md"}`)}}}, nil
		})
	}
	repoDir, headSHA := newGitRepo(t)
	runner := newRunner(&fakeModel{script: script})
	runner.SetRemote(repoDir)
	runner.SetSteps(3)

	_, err := runner.Start(t.Context(), testRequest(headSHA))
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseLimit || !errors.Is(err, pipeline.ErrLimit) {
		t.Fatalf("Start() = %v, want a *review.FailedError with cause %q wrapping pipeline.ErrLimit", err, review.CauseLimit)
	}
}
