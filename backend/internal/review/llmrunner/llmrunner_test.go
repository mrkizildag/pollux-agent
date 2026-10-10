package llmrunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// fakeModel scripts one llm.Response (or error) per call, in order, and
// records every request it saw.
type fakeModel struct {
	script []func(req llm.Request) (llm.Response, error)
	calls  []llm.Request
}

func (f *fakeModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	f.calls = append(f.calls, req)
	i := len(f.calls) - 1
	if i >= len(f.script) {
		return llm.Response{}, fmt.Errorf("fakeModel: unexpected call %d", i+1)
	}
	return f.script[i](req)
}

func triageResponse(impacted bool) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		text, err := json.Marshal(map[string]any{"impacted": impacted, "reason": "scripted"})
		if err != nil {
			return llm.Response{}, fmt.Errorf("marshal triage verdict: %w", err)
		}
		return llm.Response{Text: string(text)}, nil
	}
}

func textResponse(text string) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) { return llm.Response{Text: text}, nil }
}

func verifyResponse(supported bool) func(llm.Request) (llm.Response, error) {
	return textResponse(fmt.Sprintf(`{"supported": %t, "reason": "scripted"}`, supported))
}

func proposalFor(docPath string, line int) map[string]any {
	return map[string]any{
		"doc_path": docPath,
		"section":  "X",
		"anchor":   map[string]any{"file": "main.go", "line": line},
		"reason":   "main.go's behavior changed",
		"content":  "new behavior.",
	}
}

func submitResponse(proposals ...any) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) {
		args, err := json.Marshal(map[string]any{"proposals": proposals})
		if err != nil {
			return llm.Response{}, fmt.Errorf("marshal proposals: %w", err)
		}
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "s", Name: "submit_proposals", Args: args}}}, nil
	}
}

func testRequest(headSHA string) review.Request {
	return review.Request{
		InstallationID: 1,
		Owner:          "o",
		Repo:           "r",
		Number:         1,
		HeadSHA:        headSHA,
		BaseSHA:        headSHA,
		ChangedFiles: []review.ChangedFile{
			{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"},
		},
	}
}

// newGitRepo creates a repo with a code file and a doc, commits it, and
// returns the repo's directory and the commit's SHA.
func newGitRepo(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	run("init", "-q", "-b", "main")
	// A detached auto-maintenance can outlive the test and break TempDir cleanup.
	run("config", "maintenance.auto", "false")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("write main.go: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o700); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "x.md"), []byte("---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n# X\n\nold behavior.\n"), 0o600); err != nil {
		t.Fatalf("write docs/x.md: %v", err)
	}

	run("add", "-A")
	run("commit", "-q", "-m", "init")

	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path, not external input
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	headSHA := string(out)
	headSHA = headSHA[:len(headSHA)-1] // trim trailing newline

	return dir, headSHA
}

func noToken(context.Context, int64, string) (string, error) { return "", nil }

func newDocResponse(needed bool) func(llm.Request) (llm.Response, error) {
	return textResponse(fmt.Sprintf(`{"needed": %t, "reason": "scripted"}`, needed))
}

func otherGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "other.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -0,0 +1,3 @@\n+func other() {}\n"}
}

func newDocProposal(covers string) map[string]any {
	return map[string]any{
		"doc_path":    "docs/other.md",
		"section":     "",
		"anchor":      map[string]any{"file": "other.go", "line": 2},
		"reason":      "other.go adds a feature",
		"content":     "---\ntitle: Other\nsummary: About other.\ncovers:\n  - " + covers + "\n---\n# Other\n",
		"index_entry": "- [Other](other.md): about other.",
	}
}

func TestStart_TokenBudgetExceededDuringTriage(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{Text: `{"impacted": false, "reason": "x"}`, Usage: llm.Usage{InputTokens: 100}}, nil
		},
	}}

	repoDir, headSHA := newGitRepo(t)
	runner := newRunner(model)
	runner.SetRemote(repoDir)
	runner.SetTokenBudget(10)

	_, err := runner.Start(t.Context(), testRequest(headSHA))
	if !errors.Is(err, pipeline.ErrLimit) {
		t.Fatalf("Start() = %v, want errors.Is pipeline.ErrLimit", err)
	}
}

type blockingModel struct{}

func (blockingModel) Complete(ctx context.Context, _ llm.Request) (llm.Response, error) {
	<-ctx.Done()
	return llm.Response{}, fmt.Errorf("blocking model: %w", ctx.Err())
}

func TestStart_DeadlineIsErrDeadline(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	runner := newRunner(blockingModel{})
	runner.SetRemote(repoDir)
	runner.SetTimeout(200 * time.Millisecond)

	_, err := runner.Start(t.Context(), testRequest(headSHA))
	if !errors.Is(err, pipeline.ErrTimeout) {
		t.Fatalf("Start() = %v, want errors.Is pipeline.ErrTimeout", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Start() = %v, must not match context.Canceled", err)
	}
}

func TestStart_RejectsHeadSHAThatIsNotAFullObjectID(t *testing.T) {
	t.Parallel()

	model := &fakeModel{}
	runner := newRunner(model)

	_, err := runner.Start(t.Context(), testRequest("--upload-pack=x"))
	if err == nil {
		t.Fatal("Start(head sha \"--upload-pack=x\") = nil error, want an error")
	}
	if len(model.calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(model.calls))
	}
}

func commitDoc(t *testing.T, dir, relPath, content string) string {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "doc"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	out, err := exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "HEAD").Output() //nolint:gosec // dir is a t.TempDir path, not external input
	if err != nil {
		t.Fatalf("rev-parse HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func TestStart_DocsFileAtHeadIsAbsentReadme(t *testing.T) {
	t.Parallel()

	dir, baseSHA := newGitRepo(t)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("rm", "-rq", "docs")
	if err := os.WriteFile(filepath.Join(dir, "docs"), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("write docs file: %v", err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "docs becomes a file")
	headSHA := git("rev-parse", "HEAD")

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){newDocResponse(false)}}
	runner := newRunner(model)
	runner.SetRemote(dir)
	req := testRequest(headSHA)
	req.BaseSHA = baseSHA
	req.ChangedFiles = []review.ChangedFile{otherGoChange()}

	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	if _, ok := started.(review.Result); !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if len(model.calls) != 1 {
		t.Fatalf("model saw %d calls, want exactly 1 new-doc decision", len(model.calls))
	}
}

// startOnRepo runs Start over a repo the caller prepared and returns the
// verdict and the model.
func startOnRepo(t *testing.T, repoDir, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, *fakeModel) {
	t.Helper()

	model := &fakeModel{script: script}
	runner := newRunner(model)
	runner.SetRemote(repoDir)
	req := testRequest(headSHA)
	req.ChangedFiles = changed
	started, err := runner.Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, model
}

// returnedToModel is the error text of the tool result the model got after its
// first submission.
func returnedToModel(t *testing.T, model *fakeModel) string {
	t.Helper()

	for _, call := range model.calls {
		for _, m := range call.Messages {
			for _, r := range m.ToolResults {
				if r.IsError {
					return r.Content
				}
			}
		}
	}
	t.Fatal("model never received an error tool result")
	return ""
}

// The three smoke tests below run the whole composition over a real clone: the
// policy scenarios themselves live in the pipeline's tests.

func TestStart_ImpactedDocProducesProposal(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	readDoc := func(llm.Request) (llm.Response, error) {
		return llm.Response{
			ToolCalls: []llm.ToolCall{{ID: "1", Name: "read_file", Args: json.RawMessage(`{"path":"docs/x.md"}`)}},
			Usage:     llm.Usage{InputTokens: 20, OutputTokens: 5},
		}, nil
	}
	submit := func(req llm.Request) (llm.Response, error) {
		last := req.Messages[len(req.Messages)-1]
		if len(last.ToolResults) != 1 || last.ToolResults[0].IsError {
			t.Errorf("tool result = %+v, want a successful read_file result", last)
		}
		return submitResponse(proposalFor("docs/x.md", 2))(req)
	}
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		withUsage(triageResponse(true), llm.Usage{InputTokens: 10, OutputTokens: 1}), readDoc, submit,
		withUsage(verifyResponse(true), llm.Usage{InputTokens: 7, OutputTokens: 1}),
	}}
	runner := newRunner(model)
	runner.SetRemote(repoDir)

	started, err := runner.Start(t.Context(), testRequest(headSHA))
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	proposals, ok := result.Verdict.(review.Proposals)
	if !ok || len(proposals) != 1 {
		t.Fatalf("Verdict = %#v, want one proposal", result.Verdict)
	}
	p := proposals[0]
	if p.DocPath != "docs/x.md" || p.Anchor != (review.Anchor{File: "main.go", Line: 2}) || p.Section != "X" || !strings.Contains(p.Original, "old behavior.") {
		t.Errorf("proposals[0] = %+v, want docs/x.md anchored at main.go:2 with the section's current text", p)
	}
	if result.Model != "draft-model" {
		t.Errorf("Model = %q, want draft-model", result.Model)
	}
	want := review.Tokens{Input: 37, Output: 7}
	if result.Usage == nil || *result.Usage.Tokens != want {
		t.Errorf("Usage = %+v, want the triage, draft and verify tokens summed to %+v", result.Usage, want)
	}
}

func TestStart_InvalidProposalIsReturnedToModel(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/x.md", 2)
	bad["anchor"] = map[string]any{"file": "main.go", "line": 99}
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(bad),
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 || !last.ToolResults[0].IsError || !strings.Contains(last.ToolResults[0].Content, "proposal 0: anchor.line 99") {
				t.Errorf("last message = %+v, want a validation error tool result for proposal 0", last)
			}
			return submitResponse(proposalFor("docs/x.md", 2))(req)
		},
		verifyResponse(true),
	}}
	repoDir, headSHA := newGitRepo(t)
	runner := newRunner(model)
	runner.SetRemote(repoDir)

	started, err := runner.Start(t.Context(), testRequest(headSHA))
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if proposals, ok := result.Verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want the resubmitted valid proposal", result.Verdict)
	}
}

func TestStart_VerificationDropsRejectedProposal(t *testing.T) {
	t.Parallel()

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true),
		submitResponse(proposalFor("docs/x.md", 2), proposalFor("docs/x.md", 3)),
		verifyResponse(true),
		verifyResponse(false),
	}}
	repoDir, headSHA := newGitRepo(t)
	runner := newRunner(model)
	runner.SetRemote(repoDir)

	started, err := runner.Start(t.Context(), testRequest(headSHA))
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if proposals, ok := result.Verdict.(review.Proposals); !ok || len(proposals) != 1 || proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want exactly the first proposal", result.Verdict)
	}
}

func withUsage(next func(llm.Request) (llm.Response, error), u llm.Usage) func(llm.Request) (llm.Response, error) {
	return func(req llm.Request) (llm.Response, error) {
		resp, err := next(req)
		resp.Usage = u
		return resp, err
	}
}
