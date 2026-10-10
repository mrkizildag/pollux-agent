// Package review holds the contracts shared by the analysis runners and the
// gate: what a runner is asked to review and what it returns.
package review

import (
	"context"
	"time"
)

// Runner analyzes a pull request's docs impact.
type Runner interface {
	// Start returns a Result when the runner finishes synchronously, or Pending
	// when the Result arrives later through a webhook.
	Start(ctx context.Context, req Request) (Started, error)
}

// AsyncRunner is a Runner whose Start may return Pending; Collect produces
// the Result once the external run completes.
type AsyncRunner interface {
	Runner
	// Collect returns *InvalidResultError when the run finished but its output
	// is unusable; any other error is transient and may be retried.
	Collect(ctx context.Context, c Completion) (Result, error)
}

// Completion identifies a finished external run and what it was started for.
type Completion struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string
	BaseSHA        string // merge base the run was started at
	RunID          int64
	Nonce          string
}

// InvalidResultError means an external run finished with output that cannot
// be turned into a Result.
type InvalidResultError struct {
	Cause error
}

func (e *InvalidResultError) Error() string { return "invalid analysis result: " + e.Cause.Error() }

func (e *InvalidResultError) Unwrap() error { return e.Cause }

// FailureCause says which way a runner's analysis failed; the gate maps each
// to fixed text, so error text from a model or provider never reaches GitHub.
type FailureCause string

const (
	CauseProvider          FailureCause = "provider"
	CauseTimeout           FailureCause = "timeout"
	CauseLimit             FailureCause = "limit"
	CauseTooManyCandidates FailureCause = "too_many_candidates"
	CauseClone             FailureCause = "clone"
	CauseInternal          FailureCause = "internal"
)

// FailedError is an analysis that failed for a known Cause.
type FailedError struct {
	Cause FailureCause
	Err   error
}

func (e *FailedError) Error() string {
	return "analysis failed (" + string(e.Cause) + "): " + e.Err.Error()
}

func (e *FailedError) Unwrap() error { return e.Err }

// Started is Pending or Result.
type Started interface{ isStarted() }

// Pending means the analysis runs elsewhere as run RunID; its Result must
// arrive before Deadline.
type Pending struct {
	RunID    int64
	Nonce    string
	Deadline time.Time
}

// DroppedProposal records one rejected proposal, using its original zero-based index.
// Reason is diagnostic text from validation, never proposal content.
type DroppedProposal struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
}

// Result is a finished analysis.
type Result struct {
	// Model is the model that produced the verdict; empty when none ran.
	Model   string
	Verdict Verdict
	// Usage is what the analysis consumed; nil when the runner did not report it.
	Usage *Usage
	// Dropped are proposal-local failures discarded by an Actions analysis.
	Dropped []DroppedProposal
}

// Usage is what one analysis consumed. Tokens is nil when the runner reported
// no token count; CostUSD is nil when it has no cost figure; CostBasis says how
// a reported cost was derived ("list" for a list-price estimate) and is empty
// when unknown.
type Usage struct {
	Tokens    *Tokens
	CostUSD   *float64
	CostBasis string
}

// Tokens is a token count by kind.
type Tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

func (Pending) isStarted() {}
func (Result) isStarted()  {}

// Verdict is NoImpact or Proposals.
type Verdict interface{ isVerdict() }

// NoImpact means no doc needs to change; Reason is one line.
type NoImpact struct {
	Reason string
}

// Proposals are the doc changes the PR needs.
type Proposals []Proposal

func (NoImpact) isVerdict()  {}
func (Proposals) isVerdict() {}

// Proposal replaces one section of a doc under docs/, or creates a new doc
// when Section is empty.
type Proposal struct {
	DocPath    string `json:"doc_path" jsonschema:"Repo-relative path, under docs/, of the doc this proposal changes or creates."`
	Section    string `json:"section" jsonschema:"Heading of the section to replace, or empty to create a new doc."`
	Anchor     Anchor `json:"anchor" jsonschema:"Changed file and a head-side line number shown in the numbered diff; the review comment goes on that line."`
	Reason     string `json:"reason" jsonschema:"One-line explanation of why this doc change is needed."`
	Content    string `json:"content" jsonschema:"Full replacement for the section including its heading line, or the full content of a new doc."`
	IndexEntry string `json:"index_entry,omitempty" jsonschema:"Entry to add to the docs index; set iff section is empty."`

	// Original is the section's current text at head, heading line included;
	// empty for a new doc. Runners fill it; the model never supplies it.
	Original string `json:"-"`
	// Lines is Original's head-side line range in the doc; zero for a new doc.
	Lines LineRange `json:"-"`
}

// Anchor is the changed file and a numbered head-side diff line that caused a
// proposal; its review comment goes on that line.
type Anchor struct {
	File string `json:"file" jsonschema:"Path of the changed file the anchor points into."`
	Line int    `json:"line" jsonschema:"Head-side line number printed in the numbered diff of the file."`
}

// Request is what a runner reviews.
type Request struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	// BaseSHA is the merge base of the PR's base branch and head: the commit the PR's diff starts from.
	BaseSHA      string
	HeadSHA      string
	ChangedFiles []ChangedFile
}

// ChangedFile is a file in the PR diff and the head-side line ranges its hunks cover.
type ChangedFile struct {
	Path         string
	PreviousPath string // old path of a renamed or moved file; empty otherwise.
	Removed      bool   // the PR deletes this file.
	Hunks        []LineRange
	Patch        string // unified diff text for Path, as GitHub returns it; empty when GitHub omits it.
	// Changes is GitHub's count of added and deleted lines in the file. A file
	// with Changes > 0 and no Patch had its diff omitted; binary files have 0.
	Changes int
}

// LineRange is an inclusive range of 1-based line numbers.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}
