package llmrunner_test

import (
	"log/slog"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// testRunner is the server runner as cmd/server composes it, with the test
// overrides the scenarios need: the clone's remote and the run's limits.
type testRunner struct {
	*pipeline.Sync
	backend *llmrunner.Backend
	limits  pipeline.Limits
	log     *slog.Logger
}

// newRunner composes a runner over model with the default triage and draft models.
func newRunner(model llm.Model) *testRunner {
	return newRunnerWith(model, "triage-model", "draft-model", slog.New(slog.DiscardHandler))
}

func newRunnerWith(model llm.Model, triageModel, draftModel string, log *slog.Logger) *testRunner {
	backend := llmrunner.NewBackend(model, noToken, draftModel, log)
	sync := pipeline.NewSync(backend, llmrunner.NewJudge(model, triageModel), log)
	return &testRunner{Sync: sync, backend: backend, limits: pipeline.ReviewLimits(), log: log}
}

// SetRemote overrides the clone's remote URL.
func (r *testRunner) SetRemote(remote string) { r.backend.SetRemote(remote) }

// SetTimeout overrides the analysis deadline.
func (r *testRunner) SetTimeout(d time.Duration) {
	r.limits.Deadline = d
	r.Sync = r.WithLimits(r.limits)
}

// SetTokenBudget overrides the analysis token budget.
func (r *testRunner) SetTokenBudget(n int) {
	r.limits.Tokens = n
	r.Sync = r.WithLimits(r.limits)
}

// SetSteps overrides the draft's step limit.
func (r *testRunner) SetSteps(n int) {
	r.limits.Steps = n
	r.Sync = r.WithLimits(r.limits)
}
