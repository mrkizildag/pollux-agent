package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

// Sync implements review.Runner and review.Scaffolder: a small-model triage
// per candidate doc, a backend task that drafts proposals, and a verification
// of each; and a backend task that scaffolds a repo's docs. Candidates are the
// docs whose covers at the base commit match the changed files, so a PR can't
// opt a doc out by editing its own covers.
type Sync struct {
	backend Backend
	judge   Judge
	log     *slog.Logger
	limits  Limits
}

var (
	_ review.Runner     = (*Sync)(nil)
	_ review.Scaffolder = (*Sync)(nil)
)

// NewSync returns a Sync that drafts with b and triages and verifies with j.
func NewSync(b Backend, j Judge, log *slog.Logger) *Sync {
	return &Sync{backend: b, judge: j, log: log, limits: ReviewLimits()}
}

// WithLimits returns a copy of s that runs under l instead of ReviewLimits.
func (s *Sync) WithLimits(l Limits) *Sync {
	c := *s
	c.limits = l
	return &c
}

// Start implements review.Runner. Every error it returns is a
// *review.FailedError whose Err keeps the original chain. It logs one
// "analysis done" record per call.
func (s *Sync) Start(ctx context.Context, req review.Request) (review.Started, error) {
	log := s.log.With("repo", req.Owner+"/"+req.Repo, "pr", req.Number, "head_sha", req.HeadSHA)
	if len(req.ChangedFiles) == 0 {
		res := noImpact("no changed files", "", nil)
		logDone(log, res, nil, nil)
		return res, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.limits.Deadline)
	defer cancel()

	meter := NewMeter(s.limits.Tokens)
	res, err := s.analyze(ctx, req, log, meter)
	if err != nil {
		failure := failed(fmt.Errorf("start analysis %s/%s#%d: %w", req.Owner, req.Repo, req.Number, err))
		logDone(log, review.Result{}, meter, failure)
		return nil, failure
	}
	logDone(log, res, meter, nil)
	return res, nil
}

// logDone writes the one summary record of a finished analysis.
func logDone(log *slog.Logger, res review.Result, m *Meter, failure *review.FailedError) {
	attrs := doneAttrs(res.Model, m, failure)
	if failure == nil {
		outcome := "no_impact"
		if isProposals(res.Verdict) {
			outcome = "proposals"
		}
		attrs = append(attrs, "outcome", outcome)
	}
	log.Info("analysis done", attrs...)
}

// doneAttrs are the summary attrs both runs share: the model, the token totals
// and, for a failure, its outcome and cause. They never carry file content or
// model text.
func doneAttrs(model string, m *Meter, failure *review.FailedError) []any {
	var tokens review.Tokens
	if u := m.Usage(); u != nil {
		tokens = *u.Tokens
	}
	attrs := []any{
		"model", model,
		"input_tokens", tokens.Input,
		"output_tokens", tokens.Output,
		"cache_read_tokens", tokens.CacheRead,
		"cache_write_tokens", tokens.CacheWrite,
	}
	if failure != nil {
		attrs = append(attrs, "outcome", "failed", "cause", string(failure.Cause))
	}
	return attrs
}

func isProposals(v review.Verdict) bool {
	_, ok := v.(review.Proposals)
	return ok
}

// failed classifies err into the *review.FailedError a runner returns.
func failed(err error) *review.FailedError {
	if errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrTimeout) {
		err = fmt.Errorf("%w: %w", ErrTimeout, err)
	}
	return &review.FailedError{Cause: classify(err), Err: err}
}

func classify(err error) review.FailureCause {
	switch {
	case errors.Is(err, ErrLimit):
		return review.CauseLimit
	case errors.Is(err, ErrTimeout):
		return review.CauseTimeout
	case errors.Is(err, errTooManyCandidates):
		return review.CauseTooManyCandidates
	case errors.Is(err, ErrWorkspace):
		return review.CauseClone
	case errors.Is(err, ErrProvider):
		return review.CauseProvider
	default:
		return review.CauseInternal
	}
}

// noImpact is the Result for a PR that needs no doc change, decided by model;
// pass "" and a nil meter when no model was called.
func noImpact(reason, model string, m *Meter) review.Result {
	return review.Result{Model: model, Verdict: review.NoImpact{Reason: reason}, Usage: m.Usage()}
}

func (s *Sync) analyze(ctx context.Context, req review.Request, log *slog.Logger, meter *Meter) (review.Result, error) {
	session, err := s.backend.Open(ctx, Checkout{
		InstallationID: req.InstallationID, Owner: req.Owner, Repo: req.Repo, Head: req.HeadSHA, Base: req.BaseSHA,
	})
	if err != nil {
		return review.Result{}, fmt.Errorf("open checkout: %w", err)
	}
	defer session.Close()

	selection, err := selectAtBase(ctx, session, req)
	if err != nil {
		return review.Result{}, err
	}
	if len(selection.Restores) > 0 {
		return review.Result{Verdict: review.Proposals(selection.Restores)}, nil
	}
	in := input.New(req, selection)
	if len(in.Candidates) == 0 && len(in.Uncovered) == 0 {
		return noImpact(basedocs.NothingToReview, "", nil), nil
	}
	if len(in.Candidates) > basedocs.MaxCandidates {
		return review.Result{}, fmt.Errorf("%w: %d candidate docs exceed the cap of %d", errTooManyCandidates, len(in.Candidates), basedocs.MaxCandidates)
	}
	candidates, err := candidateDocs(ctx, session, in.Candidates)
	if err != nil {
		return review.Result{}, fmt.Errorf("docs of %s: %w", req.HeadSHA, err)
	}

	r, err := s.newRun(session, req, log, meter)
	if err != nil {
		return review.Result{}, err
	}
	rules := finalize.Rules{Changed: req.ChangedFiles, Selection: &selection, Repo: req.Owner + "/" + req.Repo}
	return r.decide(ctx, in, candidates, rules)
}

// decide runs the model stages over the selected candidates. It sets
// rules.AllowNewDoc from the new-doc stage.
func (r run) decide(ctx context.Context, in input.Input, candidates []docs.Doc, rules finalize.Rules) (review.Result, error) {
	triageModel := r.s.judge.Model()

	impacted, reasons, err := r.triageAll(ctx, candidates)
	if err != nil {
		return review.Result{}, err
	}
	if len(in.Uncovered) > 0 {
		var why string
		rules.AllowNewDoc, why, err = r.newDocAllowed(ctx, in.Uncovered)
		if err != nil {
			return review.Result{}, err
		}
		if !rules.AllowNewDoc {
			reasons = append(reasons, "no doc covers "+strings.Join(in.Uncovered, ", ")+"; no new doc needed: "+why)
		}
	}
	if len(impacted) == 0 && !rules.AllowNewDoc {
		prefix := ""
		if len(in.Candidates) > 0 {
			prefix = "no candidate doc is affected: "
		}
		return noImpact(finalize.NoImpactReason(prefix+strings.Join(reasons, "; ")), triageModel, r.meter), nil
	}

	prompt := draftPrompt{impacted: impacted, files: in.Files}
	if rules.AllowNewDoc {
		prompt.newDocFiles = in.Uncovered
	}
	proposals, model, err := r.draft(ctx, rules, prompt)
	if err != nil {
		return review.Result{}, err
	}
	if len(proposals) == 0 {
		return noImpact("model proposed no doc changes", model, r.meter), nil
	}

	kept, rejected, err := r.verifyAll(ctx, proposals)
	if err != nil {
		return review.Result{}, err
	}
	if len(kept) == 0 {
		return noImpact(finalize.NoImpactReason("verification rejected every proposal: "+strings.Join(rejected, "; ")), triageModel, r.meter), nil
	}
	return review.Result{Model: model, Verdict: review.Proposals(kept), Usage: r.meter.Usage()}, nil
}

// selectAtBase picks the candidate docs and uncovered files from the docs at
// the PR's merge base.
func selectAtBase(ctx context.Context, session Session, req review.Request) (basedocs.Selection, error) {
	baseFS, err := session.BaseDocs(ctx)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("%w: %w", ErrWorkspace, err)
	}
	selection, err := basedocs.Select(baseFS, req.ChangedFiles)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("base docs of %s: %w", req.BaseSHA, err)
	}
	return selection, nil
}

// candidateDocs reads each candidate doc at the head, in order.
func candidateDocs(ctx context.Context, head finalize.Head, paths []string) ([]docs.Doc, error) {
	out := make([]docs.Doc, len(paths))
	for i, p := range paths {
		d, err := headDoc(ctx, head, p)
		if err != nil {
			return nil, err
		}
		out[i] = d
	}
	return out, nil
}

// headDoc reads docPath at the head. A doc whose frontmatter is broken is still
// returned, parsed by its body, so a PR can't opt a doc out of triage by
// breaking it.
func headDoc(ctx context.Context, head finalize.Head, docPath string) (docs.Doc, error) {
	src, ok, err := finalize.ReadDoc(ctx, head, docPath)
	if err != nil {
		return docs.Doc{}, fmt.Errorf("candidate doc %s: %w", docPath, err)
	}
	if !ok {
		return docs.Doc{}, fmt.Errorf("candidate doc %s at head is missing, not a regular file, or over %d bytes", docPath, docs.MaxDocBytes)
	}
	if d, err := docs.ParseDoc(docPath, src); err == nil {
		return d, nil
	}
	return docs.ParseBody(docPath, src), nil
}
