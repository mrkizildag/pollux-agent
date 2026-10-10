package pipeline_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

const scaffoldIndex = "---\ntitle: Docs\nsummary: Index of the docs.\ncovers: []\n---\n# Docs\n\n- [Architecture](architecture.md)\n- [Setup](guides/setup.md)\n"

const scaffoldArchitecture = "---\ntitle: Architecture\nsummary: How the repo fits together.\ncovers:\n  - main.go\n---\n# Architecture\n"

const scaffoldSetup = "---\ntitle: Setup\nsummary: How to run it.\ncovers: []\n---\n# Setup\n"

func scaffoldSubmission(index string) json.RawMessage {
	raw, err := json.Marshal(review.ScaffoldDocs{Index: index, Architecture: scaffoldArchitecture, Setup: scaffoldSetup})
	if err != nil {
		panic("marshal scaffold docs: " + err.Error())
	}
	return raw
}

func startScaffold(t *testing.T, b *fakeBackend) (review.ScaffoldStarted, *logCapture, error) {
	t.Helper()
	b.ws = newWorkspace()
	runner, c := newCapturedSync(b, newJudge())
	started, err := runner.StartScaffold(t.Context(), review.ScaffoldRequest{InstallationID: 1, Owner: "o", Repo: "r", BaseSHA: strings.Repeat("b", 40)})
	if err != nil {
		return nil, c, fmt.Errorf("start scaffold: %w", err)
	}
	return started, c, nil
}

func assertScaffoldRecord(t *testing.T, c *logCapture, outcome string) map[string]any {
	t.Helper()
	rec := c.done(t, "scaffold done")
	if rec["outcome"] != outcome {
		t.Errorf("outcome = %v, want %s", rec["outcome"], outcome)
	}
	if rec["repo"] != "o/r" || rec["scaffold_sha"] != strings.Repeat("b", 40) {
		t.Errorf("record repo/scaffold_sha = %v/%v", rec["repo"], rec["scaffold_sha"])
	}
	return rec
}

func TestStartScaffoldWritesValidDocs(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{model: "scaffold-model", submissions: []json.RawMessage{scaffoldSubmission(scaffoldIndex)}, charge: review.Tokens{Input: 9, Output: 3}}
	started, c, err := startScaffold(t, b)
	if err != nil {
		t.Fatalf("StartScaffold() = %v, want nil error", err)
	}
	want := review.Scaffold{Runner: "fake", Model: "scaffold-model", Index: scaffoldIndex, Architecture: scaffoldArchitecture, Setup: scaffoldSetup}
	if diff := cmp.Diff(want, started); diff != "" {
		t.Errorf("StartScaffold() (-want +got):\n%s", diff)
	}

	rec := assertScaffoldRecord(t, c, "written")
	if rec["model"] != "scaffold-model" {
		t.Errorf("model = %v, want scaffold-model", rec["model"])
	}
	if diff := cmp.Diff(review.Tokens{Input: 9, Output: 3}, wantTokens(rec)); diff != "" {
		t.Errorf("tokens (-want +got):\n%s", diff)
	}
}

func TestStartScaffoldReturnsBrokenIndexAsFeedback(t *testing.T) {
	t.Parallel()

	broken := strings.ReplaceAll(scaffoldIndex, "](guides/setup.md)", "](elsewhere.md)")
	b := &fakeBackend{model: "scaffold-model", submissions: []json.RawMessage{scaffoldSubmission(broken), scaffoldSubmission(scaffoldIndex)}}
	started, c, err := startScaffold(t, b)
	if err != nil {
		t.Fatalf("StartScaffold() = %v, want nil error", err)
	}
	got, ok := started.(review.Scaffold)
	if !ok || got.Index != scaffoldIndex {
		t.Fatalf("StartScaffold() = %#v, want the fixed index accepted", started)
	}
	if fb := b.lastFeedback(t); !strings.Contains(fb, "guides/setup.md") {
		t.Errorf("feedback = %q, want it to name the missing link", fb)
	}
	assertScaffoldRecord(t, c, "written")
}

func TestStartScaffoldLimit(t *testing.T) {
	t.Parallel()

	b := &fakeBackend{runErr: pipeline.ErrLimit}
	_, c, err := startScaffold(t, b)
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseLimit {
		t.Fatalf("StartScaffold() = %v, want a CauseLimit *review.FailedError", err)
	}
	rec := assertScaffoldRecord(t, c, "failed")
	if rec["cause"] != string(review.CauseLimit) {
		t.Errorf("cause = %v, want %s", rec["cause"], review.CauseLimit)
	}
}

func TestStartScaffoldFailureNamesItsCause(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		b    *fakeBackend
		want review.FailureCause
	}{
		{name: "clone failure", b: &fakeBackend{openErr: fmt.Errorf("clone: %w", pipeline.ErrWorkspace)}, want: review.CauseClone},
		{name: "provider error", b: &fakeBackend{runErr: fmt.Errorf("run agent: %w", pipeline.ErrProvider)}, want: review.CauseProvider},
		{name: "timeout", b: &fakeBackend{runErr: fmt.Errorf("run agent: %w", pipeline.ErrTimeout)}, want: review.CauseTimeout},
		{name: "token limit", b: &fakeBackend{charge: review.Tokens{Input: 600_001}}, want: review.CauseLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, c, err := startScaffold(t, tc.b)
			var failed *review.FailedError
			if !errors.As(err, &failed) || failed.Cause != tc.want {
				t.Fatalf("StartScaffold() = %v, want a %s *review.FailedError", err, tc.want)
			}
			if rec := assertScaffoldRecord(t, c, "failed"); rec["cause"] != string(tc.want) {
				t.Errorf("cause = %v, want %s", rec["cause"], tc.want)
			}
		})
	}
}
