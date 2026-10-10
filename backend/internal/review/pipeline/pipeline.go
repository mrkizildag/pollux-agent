// Package pipeline is the runner-agnostic half of the server's analysis: it
// selects candidate docs at the merge base, triages them, drafts proposals,
// finalizes and verifies them, and classifies failures. Backends move the
// bytes: they open a checkout and run the model-driven draft task over it.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
)

var (
	// ErrProvider means a model call failed or returned an unusable reply.
	ErrProvider = errors.New("model call failed or returned an unusable reply")
	// ErrTimeout means the run's deadline passed.
	ErrTimeout = errors.New("deadline exceeded")
	// ErrLimit means the run reached its step or token cap.
	ErrLimit = errors.New("step or token limit reached")
	// ErrWorkspace means the checkout could not be opened or read.
	ErrWorkspace = errors.New("workspace unavailable")

	errTooManyCandidates = errors.New("too many candidate docs")
)

// Caps on one review run, sized from measured runs on the Gemini free tier
// (1-3 steps, at most 46k tokens).
const (
	reviewSteps    = 12
	reviewTokens   = 120_000
	reviewDeadline = 150 * time.Second
)

// MaxDocBytes is the largest doc a Workspace reads or lists.
const MaxDocBytes = docs.MaxDocBytes

// Head reads the PR's head commit one path at a time.
type Head = finalize.Head

// Kind is what a Workspace finds at one path; the constants are its values.
type Kind = finalize.Kind

const (
	Missing = finalize.Missing
	Dir     = finalize.Dir
	Other   = finalize.Other
)

// Workspace is the PR's checkout as the pipeline reads it: the head commit one
// path at a time and the docs at the merge base.
type Workspace interface {
	Head
	// BaseDocs returns docs/ at the merge base as an fs.FS rooted at the repo
	// root, holding only regular .md files within docs.MaxDocBytes.
	BaseDocs(ctx context.Context) (fs.FS, error)
}

// Checkout names the commit a backend opens. Base is empty when only the head
// is needed.
type Checkout struct {
	InstallationID int64
	Owner, Repo    string
	Head, Base     string
}

// Limits bound one task. Tokens are enforced by the task's Meter and Deadline
// by the context the pipeline passes to the backend.
type Limits struct {
	Steps    int
	Tokens   int
	Deadline time.Duration
}

// ReviewLimits are the caps on one review run.
func ReviewLimits() Limits {
	return Limits{Steps: reviewSteps, Tokens: reviewTokens, Deadline: reviewDeadline}
}

// Finish is the tool the model calls once to submit its result.
type Finish struct {
	Name, Description string
	Schema            json.RawMessage
}

// Accept checks a submission. A non-nil feedback goes back to the model and the
// run continues; a non-nil fatal ends the run, and the backend's Run returns an
// error for which errors.Is(err, fatal) holds.
type Accept func(ctx context.Context, raw json.RawMessage) (feedback, fatal error)

// Task is one model-driven run over a Workspace.
type Task struct {
	System, Prompt string
	Finish         Finish
	Accept         Accept
	Limits         Limits
	Meter          *Meter
	// Log receives the run's per-step records.
	Log *slog.Logger
}

// Output is what a finished Task produced: the accepted submission and the
// model that drafted it.
type Output struct {
	Raw   json.RawMessage
	Model string
}

// Backend opens checkouts and runs tasks over them. Errors it returns wrap
// ErrWorkspace, ErrProvider, ErrTimeout or ErrLimit.
type Backend interface {
	Name() string
	Open(ctx context.Context, c Checkout) (Workspace, func(), error)
	Run(ctx context.Context, ws Workspace, t Task) (Output, error)
}

// Question is one tool-less prompt to the Judge. Log receives the call's record.
type Question struct {
	Kind, System, Prompt string
	Log                  *slog.Logger
}

// Judge answers the small tool-less questions of triage, new-doc and
// verification, charging m for each call.
type Judge interface {
	Model() string
	Ask(ctx context.Context, q Question, m *Meter) (string, error)
}
