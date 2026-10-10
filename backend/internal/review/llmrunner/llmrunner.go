// Package llmrunner is the server's pipeline.Backend and pipeline.Judge: a
// depth-1 git clone of the pull request's head commit, an agent loop over it,
// and the small-model calls, all served by one llm.Model. It decides nothing
// about what a valid analysis is; that is the pipeline's.
package llmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"syscall"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

const runnerName = "llmrunner"

// Backend implements pipeline.Backend over a clone and an agent loop.
type Backend struct {
	m     llm.Model
	token func(ctx context.Context, installationID int64, repo string) (string, error)
	model string

	// Test override; see export_test.go.
	remote string
}

var _ pipeline.Backend = (*Backend)(nil)

// NewBackend returns a Backend that drafts with model, served by m, and
// authenticates clones with a token from token.
func NewBackend(m llm.Model, token func(ctx context.Context, installationID int64, repo string) (string, error), model string) *Backend {
	return &Backend{m: m, token: token, model: model}
}

// Name implements pipeline.Backend.
func (b *Backend) Name() string { return runnerName }

// Open implements pipeline.Backend. Its errors wrap pipeline.ErrWorkspace.
func (b *Backend) Open(ctx context.Context, ck pipeline.Checkout) (pipeline.Session, error) {
	c, err := b.openClone(ctx, ck)
	if err != nil {
		return nil, err
	}
	return &session{c: c, base: ck.Base, m: b.m, model: b.model}, nil
}

// session is a clone opened for one analysis: the head as a pipeline.Head, the
// docs at the base commit the clone also fetched, and the agent loop over it.
// Its Stat never follows a symlink at the path it is asked about.
type session struct {
	c     *clone
	base  string
	m     llm.Model
	model string
}

var _ pipeline.Session = (*session)(nil)

// Close implements pipeline.Session.
func (s *session) Close() { s.c.close() }

// BaseDocs implements pipeline.Session.
func (s *session) BaseDocs(ctx context.Context) (fs.FS, error) {
	if s.base == "" {
		return nil, fmt.Errorf("base docs: %w: checkout has no base commit", pipeline.ErrWorkspace)
	}
	return s.c.docsAt(ctx, s.base)
}

// Run implements pipeline.Session: an agent loop over the clone offering
// t.Finish, ended by a submission t.Accept takes. A fatal error from t.Accept
// ends the run and is wrapped in Run's error.
func (s *session) Run(ctx context.Context, t pipeline.Task) (pipeline.Output, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var fatal error
	raw, _, err := agent.Run(runCtx, s.m, agent.Task{
		Model:  s.model,
		System: t.System,
		Prompt: t.Prompt,
		Root:   s.c.root,
		Finish: llm.Tool{Name: t.Finish.Name, Description: t.Finish.Description, Schema: t.Finish.Schema},
		Accept: func(args json.RawMessage) error {
			feedback, fatalErr := t.Accept(runCtx, args)
			if fatalErr != nil {
				// The model cannot fix a failed read of the clone: end the loop.
				fatal = fatalErr
				cancel()
				return errors.New("the server could not read the repository; submission not accepted")
			}
			return feedback //nolint:wrapcheck // the model reads the problems verbatim
		},
		MaxSteps: t.Limits.Steps,
		Log:      t.Log,
	}, meterCharger{t.Meter})
	if fatal != nil {
		return pipeline.Output{}, fmt.Errorf("run agent: %w", fatal)
	}
	if err != nil {
		return pipeline.Output{}, mapAgentError(err)
	}
	return pipeline.Output{Raw: raw, Model: s.model}, nil
}

// mapAgentError wraps err in the pipeline sentinel for its cause, keeping err.
func mapAgentError(err error) error {
	switch {
	case errors.Is(err, agent.ErrStepLimit):
		return fmt.Errorf("run agent: %w: %w", pipeline.ErrLimit, err)
	case errors.Is(err, agent.ErrDeadline):
		return fmt.Errorf("run agent: %w: %w", pipeline.ErrTimeout, err)
	case errors.Is(err, pipeline.ErrLimit):
		return fmt.Errorf("run agent: %w", err)
	default:
		return fmt.Errorf("run agent: %w: %w", pipeline.ErrProvider, err)
	}
}

// meterCharger bills an agent run's usage to a pipeline.Meter.
type meterCharger struct{ m *pipeline.Meter }

func (c meterCharger) Charge(u llm.Usage) error {
	if err := c.m.Charge(tokensOf(u)); err != nil {
		return fmt.Errorf("charge meter: %w", err)
	}
	return nil
}

func tokensOf(u llm.Usage) review.Tokens {
	return review.Tokens{
		Input:      int64(u.InputTokens),
		Output:     int64(u.OutputTokens),
		CacheRead:  int64(u.CacheReadTokens),
		CacheWrite: int64(u.CacheWriteTokens),
	}
}

// Stat implements pipeline.Head.
func (s *session) Stat(ctx context.Context, path string) (pipeline.Kind, error) {
	info, err := s.c.root.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return pipeline.Missing, nil
	case err != nil:
		return pipeline.Missing, fmt.Errorf("lstat %s at head: %w", path, err)
	case !info.IsDir():
		return pipeline.Other, nil
	}
	// A clone checks a submodule out as an empty directory.
	if !s.emptyDir(path) {
		return pipeline.Dir, nil
	}
	isLink, err := s.c.isGitlink(ctx, path)
	if err != nil {
		return pipeline.Missing, err
	}
	if isLink {
		return pipeline.Other, nil
	}
	return pipeline.Dir, nil
}

// emptyDir reports whether dir has no entries.
func (s *session) emptyDir(dir string) bool {
	f, err := s.c.root.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }() // read-only handle
	entries, err := f.ReadDir(1)
	return errors.Is(err, io.EOF) && len(entries) == 0
}

// ReadFile implements pipeline.Head.
func (s *session) ReadFile(_ context.Context, path string) ([]byte, bool, error) {
	info, err := s.c.root.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("lstat %s at head: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Size() > pipeline.MaxDocBytes {
		return nil, false, nil
	}

	f, err := s.c.root.Open(path)
	if err != nil {
		return nil, false, fmt.Errorf("open %s at head: %w", path, err)
	}
	defer func() { _ = f.Close() }() // read-only handle

	src, err := io.ReadAll(io.LimitReader(f, pipeline.MaxDocBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("read %s at head: %w", path, err)
	}
	if len(src) > pipeline.MaxDocBytes {
		return nil, false, nil
	}
	return src, true, nil
}
