package pipeline_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// logCapture is the JSON records a Sync wrote, with the raw output for leak checks.
type logCapture struct {
	raw     bytes.Buffer
	records []map[string]any
}

func newCapturedSync(b pipeline.Backend, j pipeline.Judge) (*pipeline.Sync, *logCapture) {
	c := &logCapture{}
	return pipeline.NewSync(b, j, slog.New(slog.NewJSONHandler(&c.raw, nil))), c
}

// parse decodes the captured lines into records.
func (c *logCapture) parse(t *testing.T) []map[string]any {
	t.Helper()
	c.records = nil
	for _, line := range strings.Split(strings.TrimSpace(c.raw.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		c.records = append(c.records, rec)
	}
	return c.records
}

// done is the single record with msg, failing when there is not exactly one.
func (c *logCapture) done(t *testing.T, msg string) map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range c.parse(t) {
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	if len(out) != 1 {
		t.Fatalf("got %d %q records, want 1; log:\n%s", len(out), msg, c.raw.String())
	}
	return out[0]
}

func (c *logCapture) assertNoLeak(t *testing.T, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(c.raw.String(), s) {
			t.Errorf("log contains %q:\n%s", s, c.raw.String())
		}
	}
}

func wantTokens(rec map[string]any) review.Tokens {
	n := func(k string) int64 {
		v, _ := rec[k].(float64)
		return int64(v)
	}
	return review.Tokens{Input: n("input_tokens"), Output: n("output_tokens"), CacheRead: n("cache_read_tokens"), CacheWrite: n("cache_write_tokens")}
}

const (
	secretDocText  = "SECRET-DOC-BODY-7731"
	secretReason   = "SECRET-TRIAGE-REASON-4410"
	secretContent  = "SECRET-PROPOSAL-CONTENT-9082"
	secretProposal = "SECRET-PROPOSAL-REASON-5526"
)

func runLogged(t *testing.T, ws *fakeWorkspace, judge *fakeJudge, backend *fakeBackend, changed ...review.ChangedFile) (*logCapture, error) {
	t.Helper()
	if len(changed) == 0 {
		changed = []review.ChangedFile{mainGoChange()}
	}
	backend.ws = ws
	if backend.model == "" {
		backend.model = "draft-model"
	}
	runner, c := newCapturedSync(backend, judge)
	if _, err := runner.Start(t.Context(), request(changed...)); err != nil {
		return c, fmt.Errorf("start: %w", err)
	}
	return c, nil
}

func assertRunAttrs(t *testing.T, rec map[string]any) {
	t.Helper()
	for k, want := range map[string]any{"repo": "o/r", "pr": float64(1), "head_sha": strings.Repeat("h", 40)} {
		if rec[k] != want {
			t.Errorf("record %s = %v, want %v", k, rec[k], want)
		}
	}
}

func TestStartLogsOneAnalysisDone(t *testing.T) {
	t.Parallel()

	secretDoc := docWithCovers("\n  - main.go", secretDocText)
	triageReply := text(fmt.Sprintf(`{"impacted": false, "reason": %q}`, secretReason)).withTokens(review.Tokens{Input: 10, Output: 2})
	impacted := text(fmt.Sprintf(`{"impacted": true, "reason": %q}`, secretReason)).withTokens(review.Tokens{Input: 10, Output: 2})
	verified := text(fmt.Sprintf(`{"supported": true, "reason": %q}`, secretReason)).withTokens(review.Tokens{Input: 5, Output: 1, CacheRead: 3})
	proposal := proposalFor("docs/x.md", 2)
	proposal["content"] = secretContent
	proposal["reason"] = secretProposal

	tests := []struct {
		name      string
		judge     *fakeJudge
		backend   *fakeBackend
		changed   []review.ChangedFile
		outcome   string
		model     string
		tokens    review.Tokens
		cause     string
		wantError bool
	}{
		{
			name:    "no changed files",
			judge:   newJudge(),
			backend: &fakeBackend{},
			outcome: "no_impact",
		},
		{
			name:    "triage rejects every doc",
			judge:   newJudge().on(pipeline.KindTriage, triageReply),
			backend: &fakeBackend{},
			outcome: "no_impact",
			model:   "triage-model",
			tokens:  review.Tokens{Input: 10, Output: 2},
		},
		{
			name:    "proposals",
			judge:   newJudge().on(pipeline.KindTriage, impacted).on(pipeline.KindVerify, verified),
			backend: &fakeBackend{submissions: []json.RawMessage{submit(proposal)}, charge: review.Tokens{Input: 100, Output: 20, CacheWrite: 7}},
			outcome: "proposals",
			model:   "draft-model",
			tokens:  review.Tokens{Input: 115, Output: 23, CacheRead: 3, CacheWrite: 7},
		},
		{
			name:      "failure",
			judge:     newJudge().on(pipeline.KindTriage, impacted),
			backend:   &fakeBackend{charge: review.Tokens{Input: 4, Output: 1}, runErr: pipeline.ErrLimit},
			outcome:   "failed",
			cause:     string(review.CauseLimit),
			tokens:    review.Tokens{Input: 14, Output: 3},
			wantError: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ws := newWorkspace().doc("docs/x.md", secretDoc)
			var c *logCapture
			var err error
			if tc.name == "no changed files" {
				backend := tc.backend
				backend.ws = ws
				runner, capture := newCapturedSync(backend, tc.judge)
				_, err = runner.Start(t.Context(), request())
				c = capture
			} else {
				c, err = runLogged(t, ws, tc.judge, tc.backend, tc.changed...)
			}
			if (err != nil) != tc.wantError {
				t.Fatalf("Start() error = %v, wantError %t", err, tc.wantError)
			}

			rec := c.done(t, "analysis done")
			assertRunAttrs(t, rec)
			if rec["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", rec["outcome"], tc.outcome)
			}
			if tc.cause == "" {
				if _, ok := rec["cause"]; ok {
					t.Errorf("cause = %v, want none", rec["cause"])
				}
			} else if rec["cause"] != tc.cause {
				t.Errorf("cause = %v, want %s", rec["cause"], tc.cause)
			}
			if !tc.wantError && rec["model"] != tc.model {
				t.Errorf("model = %v, want %q", rec["model"], tc.model)
			}
			if diff := cmp.Diff(tc.tokens, wantTokens(rec)); diff != "" {
				t.Errorf("tokens (-want +got):\n%s", diff)
			}
			c.assertNoLeak(t, secretDocText, secretReason, secretContent, secretProposal)
		})
	}
}
