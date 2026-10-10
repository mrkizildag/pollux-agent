package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// Caps on one scaffold run, which reads the whole repo rather than one diff.
const (
	scaffoldSteps    = 40
	scaffoldTokens   = 600_000
	scaffoldDeadline = 8 * time.Minute
)

// StartScaffold implements review.Scaffolder: a backend task over a depth-1
// checkout of req.BaseSHA writes the three scaffold docs. It always returns a
// review.Scaffold; every error is a *review.FailedError whose Err keeps the
// original chain. It logs one "scaffold done" record per call.
func (s *Sync) StartScaffold(ctx context.Context, req review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	ctx, cancel := context.WithTimeout(ctx, scaffoldDeadline)
	defer cancel()

	log := s.log.With("repo", req.Owner+"/"+req.Repo, "scaffold_sha", req.BaseSHA)
	meter := NewMeter(scaffoldTokens)
	res, err := s.scaffold(ctx, req, log, meter)
	if err != nil {
		failed := Failed(fmt.Errorf("start scaffold %s/%s: %w", req.Owner, req.Repo, err))
		logScaffoldDone(log, "", meter, failed)
		return nil, failed
	}
	logScaffoldDone(log, res.Model, meter, nil)
	return res, nil
}

func (s *Sync) scaffold(ctx context.Context, req review.ScaffoldRequest, log *slog.Logger, meter *Meter) (review.Scaffold, error) {
	ws, cleanup, err := s.backend.Open(ctx, Checkout{
		InstallationID: req.InstallationID, Owner: req.Owner, Repo: req.Repo, Head: req.BaseSHA,
	})
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("open checkout: %w", err)
	}
	defer cleanup()

	finish, err := submitDocsFinish()
	if err != nil {
		return review.Scaffold{}, err
	}
	system, prompt, err := ScaffoldPrompts(req.Owner, req.Repo, req.BaseSHA)
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("build scaffold prompts: %w", err)
	}

	out, err := s.backend.Run(ctx, ws, Task{
		System: system,
		Prompt: prompt,
		Finish: finish,
		Accept: checkSubmittedDocs(req.Owner + "/" + req.Repo),
		Limits: Limits{Steps: scaffoldSteps, Tokens: scaffoldTokens, Deadline: scaffoldDeadline},
		Meter:  meter,
		Log:    log,
	})
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("write docs: %w", err)
	}

	var submitted review.ScaffoldDocs
	if err := json.Unmarshal(out.Raw, &submitted); err != nil {
		return review.Scaffold{}, fmt.Errorf("decode accepted submit_docs arguments: %w", err)
	}
	return review.Scaffold{Runner: s.backend.Name(), Model: out.Model, Index: submitted.Index, Architecture: submitted.Architecture, Setup: submitted.Setup}, nil
}

// logScaffoldDone writes the one summary record of a finished scaffold run.
func logScaffoldDone(log *slog.Logger, model string, m *Meter, failed *review.FailedError) {
	attrs := doneAttrs(model, m, failed)
	if failed == nil {
		attrs = append(attrs, "outcome", "written")
	}
	log.Info("scaffold done", attrs...)
}

// checkSubmittedDocs is the finishing tool's Accept for repo ("owner/repo"); a
// broken submission goes back to the model as feedback.
func checkSubmittedDocs(repo string) Accept {
	return func(_ context.Context, raw json.RawMessage) (feedback, fatal error) {
		var d review.ScaffoldDocs
		if err := json.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("decode submit_docs arguments: %w", err), nil
		}
		if err := docs.CheckScaffold(d.Index, d.Architecture, d.Setup, repo); err != nil {
			return fmt.Errorf("submit_docs: %w", err), nil
		}
		return nil, nil
	}
}

func submitDocsFinish() (Finish, error) {
	schema, err := review.ScaffoldSchema()
	if err != nil {
		return Finish{}, fmt.Errorf("build submit_docs schema: %w", err)
	}
	return Finish{
		Name:        "submit_docs",
		Description: "Submit the three starting docs for this repository. Call exactly once when they are final.",
		Schema:      schema,
	}, nil
}
