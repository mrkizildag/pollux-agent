// Package actions is the review runner that analyzes a pull request inside
// the target repo's own GitHub Actions workflow and reads the result back
// from the run's artifact.
package actions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

const (
	runnerName   = "actions"
	defaultModel = "claude-code"

	// Bounds on untrusted usage values; anything beyond them is not recorded.
	maxCostUSD      = 1e6
	maxTokens       = 1e12
	maxModelLen     = 200
	maxCostBasisLen = 32

	// maxCauseText bounds model-controlled text in an InvalidResultError, which
	// becomes a public check-run summary.
	maxCauseText = 200
	// maxProblemsText bounds the joined per-proposal problems of one batch.
	maxProblemsText = 1024
)

// DispatchInputs are the workflow_dispatch inputs of the pollux-agent workflow.
type DispatchInputs struct {
	HeadSHA  string
	PRNumber int
	Nonce    string
	// Input is what the run reviews: the candidate docs, the uncovered files,
	// and the PR's changed files at its merge base.
	Input input.Input
}

// WorkflowAPI is the GitHub Actions surface the runner needs.
type WorkflowAPI interface {
	// Dispatch starts the pollux-agent workflow on the repo's default branch and
	// returns the ID of the run it created.
	Dispatch(ctx context.Context, installationID int64, owner, repo string, in DispatchInputs) (runID int64, err error)
	// ResultArtifact returns the result.json bytes of the run's result artifact.
	ResultArtifact(ctx context.Context, installationID int64, owner, repo string, runID int64) ([]byte, error)
	// ListChangedFiles returns the pull request's files with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
	// FileAtRef returns the file's content at ref, or ok=false when the file
	// does not exist there or exceeds docs.MaxDocBytes.
	FileAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (content []byte, ok bool, err error)
	// PathAtRef reports what is at path itself at ref, without looking at its
	// parents: exists is false when nothing is there; dir is true for a
	// directory, false for a file of any size, symlink, or submodule.
	PathAtRef(ctx context.Context, installationID int64, owner, repo, path, ref string) (exists, dir bool, err error)
	// DocsAtRef returns the .md files under docs/ at ref, rooted at the repo root.
	DocsAtRef(ctx context.Context, installationID int64, owner, repo, ref string) (fs.FS, error)
}

// Artifact is the JSON document the workflow uploads as result.json.
type Artifact[T any] struct {
	HeadSHA string          `json:"head_sha"`
	Nonce   string          `json:"nonce"`
	Claude  ClaudeOutput[T] `json:"claude"`
}

// ClaudeOutput is the subset of `claude -p --output-format json` stdout the
// runner reads.
type ClaudeOutput[T any] struct {
	IsError          bool   `json:"is_error"`
	Subtype          string `json:"subtype"`
	TerminalReason   string `json:"terminal_reason"`
	APIErrorStatus   *int   `json:"api_error_status"`
	StructuredOutput *T     `json:"structured_output"`

	// The usage fields are raw because the artifact is untrusted and usage is
	// only recorded: a malformed one must not invalidate the result.
	ModelUsage   json.RawMessage `json:"modelUsage"`
	TotalCostUSD json.RawMessage `json:"total_cost_usd"`
	Usage        json.RawMessage `json:"usage"`
}

// Runner dispatches the repo's pollux-agent workflow and collects its result.
type Runner struct {
	api             WorkflowAPI
	timeout         time.Duration
	scaffoldTimeout time.Duration
	resultSchema    func() (*jsonschema.Resolved, error)
}

var (
	_ review.AsyncRunner     = (*Runner)(nil)
	_ review.AsyncScaffolder = (*Runner)(nil)
)

// New returns a Runner that dispatches through api and gives each review run
// timeout and each scaffold run scaffoldTimeout to complete.
func New(api WorkflowAPI, timeout, scaffoldTimeout time.Duration) *Runner {
	return &Runner{api: api, timeout: timeout, scaffoldTimeout: scaffoldTimeout, resultSchema: sync.OnceValues(loadResultSchema)}
}

// Start computes the candidate docs from the PR's base commit and dispatches
// the workflow to review them, returning review.Pending. When the PR deletes a
// candidate and a restore can be proposed, it returns the finished
// review.Result of restore proposals without dispatching.
func (r *Runner) Start(ctx context.Context, req review.Request) (review.Started, error) {
	where := fmt.Sprintf("%s/%s#%d", req.Owner, req.Repo, req.Number)

	selection, err := r.selectAtBase(ctx, req.InstallationID, req.Owner, req.Repo, req.BaseSHA, req.ChangedFiles)
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
	}
	if len(selection.Restores) > 0 {
		return review.Result{Verdict: review.Proposals(selection.Restores)}, nil
	}
	if len(selection.Candidates) > basedocs.MaxCandidates {
		return nil, &review.FailedError{
			Cause: review.CauseTooManyCandidates,
			Err:   fmt.Errorf("start actions run %s: %d candidate docs exceed the cap of %d", where, len(selection.Candidates), basedocs.MaxCandidates),
		}
	}

	if selection.Empty() {
		return review.Result{Verdict: review.NoImpact{Reason: basedocs.NothingToReview}}, nil
	}
	pending, err := r.dispatch(ctx, r.timeout, req.InstallationID, req.Owner, req.Repo,
		DispatchInputs{HeadSHA: req.HeadSHA, PRNumber: req.Number, Input: input.New(req, selection)})
	if err != nil {
		return nil, fmt.Errorf("start actions run %s: %w", where, err)
	}
	return pending, nil
}

// selectAtBase picks the candidate, uncovered, and restorable docs from the
// docs tree at sha. Only failing to read the tree is a *review.FailedError.
func (r *Runner) selectAtBase(ctx context.Context, installationID int64, owner, repo, sha string, changed []review.ChangedFile) (basedocs.Selection, error) {
	baseFS, err := r.api.DocsAtRef(ctx, installationID, owner, repo, sha)
	if err != nil {
		return basedocs.Selection{}, &review.FailedError{Cause: review.CauseClone, Err: fmt.Errorf("base %s: %w", sha, err)}
	}
	selection, err := basedocs.Select(baseFS, changed)
	if err != nil {
		return basedocs.Selection{}, fmt.Errorf("base %s: %w", sha, err)
	}
	return selection, nil
}

// StartScaffold dispatches the workflow with pr_number 0 and no docs at
// req.BaseSHA and returns review.Pending.
func (r *Runner) StartScaffold(ctx context.Context, req review.ScaffoldRequest) (review.ScaffoldStarted, error) {
	pending, err := r.dispatch(ctx, r.scaffoldTimeout, req.InstallationID, req.Owner, req.Repo,
		DispatchInputs{HeadSHA: req.BaseSHA, Input: input.New(review.Request{}, basedocs.Selection{})})
	if err != nil {
		return nil, fmt.Errorf("start actions scaffold %s/%s: %w", req.Owner, req.Repo, err)
	}
	return pending, nil
}

func (r *Runner) dispatch(ctx context.Context, timeout time.Duration, installationID int64, owner, repo string, in DispatchInputs) (review.Pending, error) {
	nonce, err := newNonce()
	if err != nil {
		return review.Pending{}, err
	}

	in.Nonce = nonce
	runID, err := r.api.Dispatch(ctx, installationID, owner, repo, in)
	if err != nil {
		return review.Pending{}, fmt.Errorf("dispatch: %w", err)
	}

	return review.Pending{RunID: runID, Nonce: nonce, Deadline: time.Now().Add(timeout)}, nil
}

// CollectScaffold decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another commit or
// dispatch, reports an error, or holds docs that fail docs.CheckScaffold.
func (r *Runner) CollectScaffold(ctx context.Context, c review.Completion) (review.Scaffold, error) {
	raw, err := r.api.ResultArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID)
	if err != nil {
		return review.Scaffold{}, fmt.Errorf("collect actions scaffold run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	var art Artifact[review.ScaffoldDocs]
	if err := json.Unmarshal(raw, &art); err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: fmt.Errorf("decode result artifact: %w", err)}
	}

	out, err := art.output(c)
	if err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: err}
	}
	if err := docs.CheckScaffold(out.Index, out.Architecture, out.Setup, c.Owner+"/"+c.Repo); err != nil {
		return review.Scaffold{}, &review.InvalidResultError{Cause: errors.New(capText(err.Error()))}
	}

	return review.Scaffold{
		Runner:       runnerName,
		Model:        art.Claude.model(),
		Index:        out.Index,
		Architecture: out.Architecture,
		Setup:        out.Setup,
	}, nil
}

// Collect decodes the completed run's result artifact. It returns
// *review.InvalidResultError when the artifact is for another head or
// dispatch, reports an error, or holds a malformed result or a proposal
// outside docs/. Proposal-local validation failures are dropped if valid
// proposals remain. Failing to list the PR's files is transient
// and returned as an ordinary error.
func (r *Runner) Collect(ctx context.Context, c review.Completion) (review.Result, error) {
	raw, err := r.api.ResultArtifact(ctx, c.InstallationID, c.Owner, c.Repo, c.RunID)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	var art Artifact[json.RawMessage]
	if err := json.Unmarshal(raw, &art); err != nil {
		return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("decode result artifact: %w", err)}
	}

	rawOutput, err := art.output(c)
	if err != nil {
		return review.Result{}, &review.InvalidResultError{Cause: err}
	}

	schema, err := r.resultSchema()
	if err != nil {
		return review.Result{}, fmt.Errorf("load actions result schema: %w", err)
	}
	var instance any
	if err := json.Unmarshal(*rawOutput, &instance); err != nil {
		return review.Result{}, &review.InvalidResultError{Cause: errors.New("decode structured output")}
	}
	if err := schema.Validate(instance); err != nil {
		// Schema errors can contain raw values; keep the artifact out of error text.
		return review.Result{}, &review.InvalidResultError{Cause: errors.New("structured output does not match the result schema")}
	}
	var out review.StructuredOutput
	if err := json.Unmarshal(*rawOutput, &out); err != nil {
		// Typed decoding also checks numbers without float64 rounding.
		return review.Result{}, &review.InvalidResultError{Cause: errors.New("decode structured output fields")}
	}
	if len(out.Proposals) > finalize.MaxProposals {
		return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("too many proposals: %d, max %d", len(out.Proposals), finalize.MaxProposals)}
	}
	for i, p := range out.Proposals {
		if err := review.ValidateDocPath(p.DocPath); err != nil {
			return review.Result{}, &review.InvalidResultError{Cause: fmt.Errorf("proposal %d: %s", i, capText(err.Error()))}
		}
	}

	result := review.Result{Model: art.Claude.model(), Usage: art.Claude.reviewUsage()}
	if len(out.Proposals) == 0 {
		if strings.TrimSpace(out.NoImpactReason) == "" {
			return review.Result{}, &review.InvalidResultError{Cause: errors.New("no proposals and an empty no_impact_reason")}
		}
		result.Verdict = review.NoImpact{Reason: finalize.NoImpactReason(out.NoImpactReason)}
		return result, nil
	}

	changed, err := r.api.ListChangedFiles(ctx, c.InstallationID, c.Owner, c.Repo, c.Number)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}

	rules := finalize.Rules{Changed: changed, Repo: c.Owner + "/" + c.Repo}
	if c.BaseSHA != "" {
		selection, err := r.selectAtBase(ctx, c.InstallationID, c.Owner, c.Repo, c.BaseSHA, changed)
		if err != nil {
			return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
		}
		rules.Selection = &selection
		rules.AllowNewDoc = true
	}

	proposals, problems, err := finalize.Proposals(ctx, headAt{api: r.api, c: c}, rules, out.Proposals)
	if err != nil {
		return review.Result{}, fmt.Errorf("collect actions run %d of %s/%s: %w", c.RunID, c.Owner, c.Repo, err)
	}
	rejected := make(map[int]bool, len(problems))
	for _, problem := range problems {
		if !rejected[problem.Index] {
			rejected[problem.Index] = true
			result.Dropped = append(result.Dropped, review.DroppedProposal{Index: problem.Index, Reason: capText(problem.Err.Error())})
		}
	}
	kept := make(review.Proposals, 0, len(proposals)-len(rejected))
	for i, p := range proposals {
		if !rejected[i] {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return review.Result{}, &review.InvalidResultError{Cause: errors.New(capTo(problems.Error(), maxProblemsText))}
	}
	result.Verdict = kept
	return result, nil
}

// The static schema checks the artifact's structure. Per-PR anchor ranges and
// new-doc rules remain proposal-local checks in finalize.
func loadResultSchema() (*jsonschema.Resolved, error) {
	raw, err := review.ResultSchema()
	if err != nil {
		return nil, fmt.Errorf("generate result schema: %w", err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("decode result schema: %w", err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("resolve result schema: %w", err)
	}
	return resolved, nil
}

// headAt reads the PR's head commit through the workflow API.
type headAt struct {
	api WorkflowAPI
	c   review.Completion
}

func (h headAt) Stat(ctx context.Context, path string) (finalize.Kind, error) {
	exists, dir, err := h.api.PathAtRef(ctx, h.c.InstallationID, h.c.Owner, h.c.Repo, path, h.c.HeadSHA)
	switch {
	case err != nil:
		return finalize.Missing, fmt.Errorf("stat %s at %s: %w", path, h.c.HeadSHA, err)
	case !exists:
		return finalize.Missing, nil
	case dir:
		return finalize.Dir, nil
	}
	return finalize.Other, nil
}

func (h headAt) ReadFile(ctx context.Context, path string) ([]byte, bool, error) {
	src, ok, err := h.api.FileAtRef(ctx, h.c.InstallationID, h.c.Owner, h.c.Repo, path, h.c.HeadSHA)
	if err != nil {
		return nil, false, fmt.Errorf("read %s at %s: %w", path, h.c.HeadSHA, err)
	}
	return src, ok, nil
}

// output returns the structured output of an artifact that belongs to c's
// dispatch and reports no error.
func (a Artifact[T]) output(c review.Completion) (*T, error) {
	if a.HeadSHA != c.HeadSHA {
		return nil, fmt.Errorf("artifact head_sha %q, want %q", a.HeadSHA, c.HeadSHA)
	}
	if a.Nonce != c.Nonce {
		return nil, errors.New("artifact nonce does not match the dispatch")
	}
	if a.Claude.IsError {
		return nil, a.Claude.failure()
	}
	if a.Claude.StructuredOutput == nil {
		return nil, errors.New("claude output has no structured_output")
	}
	return a.Claude.StructuredOutput, nil
}

// failure describes an errored run from structured fields only; the free-form
// result text is attacker-influenced and must not reach a public check run.
func (o ClaudeOutput[T]) failure() error {
	status := "none"
	if o.APIErrorStatus != nil {
		status = strconv.Itoa(*o.APIErrorStatus)
	}
	return fmt.Errorf("claude code failed: api_error_status %s (terminal_reason %s, subtype %s)",
		status, capText(o.TerminalReason), capText(o.Subtype))
}

func capText(s string) string {
	return capTo(s, maxCauseText)
}

func capTo(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "") + "..."
}

// reviewUsage is nil when the run reported no valid token count or cost.
// Negative or implausible values count as not reported. A count that is present
// but invalid drops all four, so a stored zero is always a reported zero.
func (o ClaudeOutput[T]) reviewUsage() *review.Usage {
	fields := decodeObject(o.Usage)
	tokensReported, tokensValid := false, true
	count := func(key string) int64 {
		raw, present := fields[key]
		if !present {
			return 0
		}
		n, ok := boundedNumber(raw, maxTokens)
		tokensReported = tokensReported || ok
		tokensValid = tokensValid && ok
		return int64(n)
	}
	tokens := review.Tokens{
		Input:      count("input_tokens"),
		Output:     count("output_tokens"),
		CacheRead:  count("cache_read_input_tokens"),
		CacheWrite: count("cache_creation_input_tokens"),
	}
	u := review.Usage{CostBasis: o.costBasis()}
	if tokensReported && tokensValid {
		u.Tokens = &tokens
	}
	if cost, ok := boundedNumber(o.TotalCostUSD, maxCostUSD); ok {
		u.CostUSD = &cost
	}
	if u.Tokens == nil && u.CostUSD == nil {
		return nil
	}
	return &u
}

// boundedNumber reports raw as a number in [0, limit]; JSON null is not one.
func boundedNumber(raw json.RawMessage, limit float64) (float64, bool) {
	var n *float64
	if json.Unmarshal(raw, &n) != nil || n == nil || *n < 0 || *n > limit {
		return 0, false
	}
	return *n, true
}

// decodeObject is nil unless raw is a JSON object.
func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// costBasis is the basis every model reports, or empty when they differ,
// none is given, or one is malformed or over maxCostBasisLen.
func (o ClaudeOutput[T]) costBasis() string {
	models := decodeObject(o.ModelUsage)
	basis := ""
	for i, name := range slices.Sorted(maps.Keys(models)) {
		var entry struct {
			CostBasis string `json:"costBasis"`
		}
		if json.Unmarshal(models[name], &entry) != nil || len(entry.CostBasis) > maxCostBasisLen {
			entry.CostBasis = ""
		}
		if i > 0 && entry.CostBasis != basis {
			return ""
		}
		basis = entry.CostBasis
	}
	return basis
}

func (o ClaudeOutput[T]) model() string {
	if models := slices.Sorted(maps.Keys(decodeObject(o.ModelUsage))); len(models) > 0 {
		name := models[0]
		if len(name) > maxModelLen {
			name = strings.ToValidUTF8(name[:maxModelLen], "")
		}
		return name
	}
	return defaultModel
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}
