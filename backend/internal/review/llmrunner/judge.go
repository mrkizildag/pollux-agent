package llmrunner

import (
	"context"
	"fmt"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// Judge implements pipeline.Judge with tool-less calls to a small model.
type Judge struct {
	m     llm.Model
	model string
}

var _ pipeline.Judge = (*Judge)(nil)

// NewJudge returns a Judge that asks triageModel, served by m.
func NewJudge(m llm.Model, triageModel string) *Judge {
	return &Judge{m: m, model: triageModel}
}

// Model implements pipeline.Judge.
func (j *Judge) Model() string { return j.model }

// Ask implements pipeline.Judge: one prompt to the triage model, its usage
// charged to m. Its errors wrap pipeline.ErrProvider or pipeline.ErrLimit.
func (j *Judge) Ask(ctx context.Context, q pipeline.Question, m *pipeline.Meter) (string, error) {
	resp, err := j.m.Complete(ctx, llm.Request{
		Model:    j.model,
		System:   q.System,
		Messages: []llm.Message{{Role: llm.RoleUser, Text: q.Prompt}},
	})
	if err != nil {
		return "", fmt.Errorf("complete: %w: %w", pipeline.ErrProvider, err)
	}
	q.Log.Info("agent call", "kind", q.Kind, "model", j.model,
		"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens)
	if err := m.Charge(tokensOf(resp.Usage)); err != nil {
		return "", fmt.Errorf("charge token budget: %w", err)
	}
	return resp.Text, nil
}
