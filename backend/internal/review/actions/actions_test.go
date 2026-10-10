package actions_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

type fakeAPI struct {
	dispatched actions.DispatchInputs
	runID      int64
	artifact   []byte
	err        error
	changed    []review.ChangedFile
	changedErr error
	files      map[string][]byte
	fileErr    map[string]error
	// paths are extra non-directory paths PathAtRef reports: files FileAtRef refuses, symlinks.
	paths map[string]bool
	// dirs are directories PathAtRef reports besides the parents of files.
	dirs      map[string]bool
	fileReads map[string]int
	docsAt    map[string]fstest.MapFS
	// docsFS, when set, is returned for every ref instead of docsAt.
	docsFS fs.FS
}

type unreadableFS struct{ fstest.MapFS }

func (unreadableFS) Open(string) (fs.File, error) { return nil, errors.New("unreadable") }

func (f *fakeAPI) DocsAtRef(_ context.Context, _ int64, _, _, ref string) (fs.FS, error) {
	if f.docsFS != nil {
		return f.docsFS, f.err
	}
	return f.docsAt[ref], f.err
}

func (f *fakeAPI) Dispatch(_ context.Context, _ int64, _, _ string, in actions.DispatchInputs) (int64, error) {
	f.dispatched = in
	return f.runID, f.err
}

func (f *fakeAPI) ResultArtifact(_ context.Context, _ int64, _, _ string, _ int64) ([]byte, error) {
	return f.artifact, f.err
}

func (f *fakeAPI) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return f.changed, f.changedErr
}

func (f *fakeAPI) FileAtRef(_ context.Context, _ int64, _, _, path, ref string) ([]byte, bool, error) {
	if f.fileReads == nil {
		f.fileReads = map[string]int{}
	}
	f.fileReads[path+"@"+ref]++
	src, ok := f.files[path]
	return src, ok, f.fileErr[path]
}

func (f *fakeAPI) PathAtRef(_ context.Context, _ int64, _, _, path, _ string) (exists, dir bool, err error) {
	if f.err != nil {
		return false, false, f.err
	}
	if _, isFile := f.files[path]; isFile || f.paths[path] {
		return true, false, nil
	}
	if f.dirs[path] {
		return true, true, nil
	}
	for p := range f.files {
		if strings.HasPrefix(p, path+"/") {
			return true, true, nil
		}
	}
	return false, false, nil
}

func TestStart(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 99, docsAt: map[string]fstest.MapFS{"base": {}}}
	runner := actions.New(api, 5*time.Minute, 10*time.Minute)

	before := time.Now()
	started, err := runner.Start(t.Context(), review.Request{
		InstallationID: 1, Owner: "o", Repo: "r", Number: 7, BaseSHA: "base", HeadSHA: "abc",
		ChangedFiles: []review.ChangedFile{{Path: "main.go"}},
	})
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}

	pending, ok := started.(review.Pending)
	if !ok {
		t.Fatalf("Start() = %T, want review.Pending", started)
	}
	if pending.RunID != 99 || pending.Nonce == "" || pending.Nonce != api.dispatched.Nonce {
		t.Errorf("Start() = %+v, want RunID 99 and the dispatched nonce", pending)
	}
	if pending.Deadline.Before(before.Add(5*time.Minute)) || pending.Deadline.After(time.Now().Add(5*time.Minute)) {
		t.Errorf("Start() deadline = %v, want the 5m review timeout from %v", pending.Deadline, before)
	}
	if api.dispatched.HeadSHA != "abc" || api.dispatched.PRNumber != 7 {
		t.Errorf("Dispatch inputs = %+v, want head abc, PR 7", api.dispatched)
	}
}

func TestStartDispatchesInput(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[main.go]")}}}
	req := startRequest(
		review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 2}}},
		review.ChangedFile{Path: "gone.txt", Removed: true},
	)
	if _, err := newRunner(api).Start(t.Context(), req); err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	want := input.Input{
		BaseSHA:    "base",
		Candidates: []string{"docs/a.md"},
		Uncovered:  []string{},
		Files: []input.File{
			{Path: "main.go", Ranges: []review.LineRange{{Start: 1, End: 2}}},
			{Path: "gone.txt", Ranges: []review.LineRange{}},
		},
	}
	if diff := cmp.Diff(want, api.dispatched.Input); diff != "" {
		t.Errorf("dispatched Input (-want +got):\n%s", diff)
	}
}

func newRunner(api actions.WorkflowAPI) *actions.Runner {
	return actions.New(api, time.Minute, time.Minute)
}

func coverDoc(covers string) *fstest.MapFile {
	return &fstest.MapFile{Data: []byte("---\ntitle: T\nsummary: S\ncovers: " + covers + "\n---\n# T\n")}
}

func startRequest(files ...review.ChangedFile) review.Request {
	return review.Request{InstallationID: 1, Owner: "o", Repo: "r", Number: 7, BaseSHA: "base", HeadSHA: "head", ChangedFiles: files}
}

func TestStartDispatchesBaseCandidates(t *testing.T) {
	t.Parallel()

	main := review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 2}}}

	t.Run("a doc whose head copy dropped its covers is still a candidate", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{
			"base": {"docs/a.md": coverDoc("[main.go]")},
			"head": {"docs/a.md": coverDoc("[]")},
		}}
		if _, err := newRunner(api).Start(t.Context(), startRequest(main)); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"docs/a.md"}, api.dispatched.Input.Candidates); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("a doc the PR adds is not a candidate", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{
			"base": {"docs/a.md": coverDoc("[main.go]")},
			"head": {"docs/a.md": coverDoc("[main.go]"), "docs/new.md": coverDoc("[main.go]")},
		}}
		if _, err := newRunner(api).Start(t.Context(), startRequest(main)); err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if diff := cmp.Diff([]string{"docs/a.md"}, api.dispatched.Input.Candidates); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("only uncovered files dispatch with the uncovered list and no docs", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[other.go]")}}}
		started, err := newRunner(api).Start(t.Context(), startRequest(main))
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if _, ok := started.(review.Pending); !ok || len(api.dispatched.Input.Candidates) != 0 {
			t.Errorf("Start() = %T with Docs %v, want Pending with no docs", started, api.dispatched.Input.Candidates)
		}
		if diff := cmp.Diff([]string{"main.go"}, api.dispatched.Input.Uncovered); diff != "" {
			t.Errorf("dispatched Uncovered (-want +got):\n%s", diff)
		}
	})

	t.Run("nothing candidate or uncovered concludes no impact without dispatching", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[other.go]")}}}
		started, err := newRunner(api).Start(t.Context(), startRequest(
			review.ChangedFile{Path: "docs/new.md", Hunks: []review.LineRange{{Start: 1, End: 2}}},
			review.ChangedFile{Path: "gone.go", Removed: true},
		))
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		want := review.Result{Verdict: review.NoImpact{Reason: basedocs.NothingToReview}}
		if diff := cmp.Diff(review.Started(want), started); diff != "" {
			t.Errorf("Start() (-want +got):\n%s", diff)
		}
		if api.dispatched.Nonce != "" {
			t.Errorf("Dispatch was called with %+v, want no dispatch", api.dispatched)
		}
	})

	t.Run("a renamed candidate is dispatched at its new path", func(t *testing.T) {
		t.Parallel()
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[main.go]")}}}
		req := startRequest(main, review.ChangedFile{Path: "docs/b.md", PreviousPath: "docs/a.md"})
		started, err := newRunner(api).Start(t.Context(), req)
		if err != nil {
			t.Fatalf("Start() = %v, want nil", err)
		}
		if _, ok := started.(review.Pending); !ok {
			t.Fatalf("Start() = %T, want Pending", started)
		}
		if diff := cmp.Diff([]string{"docs/b.md"}, api.dispatched.Input.Candidates); diff != "" {
			t.Errorf("dispatched Docs (-want +got):\n%s", diff)
		}
	})

	t.Run("more candidates than the cap fail without dispatching", func(t *testing.T) {
		t.Parallel()
		base := fstest.MapFS{}
		for i := range basedocs.MaxCandidates + 1 {
			base[fmt.Sprintf("docs/d%02d.md", i)] = coverDoc("[main.go]")
		}
		api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": base}}
		_, err := newRunner(api).Start(t.Context(), startRequest(main))
		var failed *review.FailedError
		if !errors.As(err, &failed) || failed.Cause != review.CauseTooManyCandidates {
			t.Fatalf("Start() = %v, want FailedError with cause %q", err, review.CauseTooManyCandidates)
		}
		if api.dispatched.Nonce != "" {
			t.Errorf("Dispatch was called with %+v, want no dispatch", api.dispatched)
		}
	})
}

func TestStartRestoresDeletedCandidate(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 1, docsAt: map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[main.go]")}}}
	req := startRequest(
		review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 4, End: 6}}},
		review.ChangedFile{Path: "docs/a.md", Removed: true},
	)

	started, err := newRunner(api).Start(t.Context(), req)
	if err != nil {
		t.Fatalf("Start() = %v, want nil", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	proposals, ok := result.Verdict.(review.Proposals)
	if !ok || len(proposals) != 1 || proposals[0].DocPath != "docs/a.md" {
		t.Fatalf("verdict = %#v, want one restore of docs/a.md", result.Verdict)
	}
	if result.Model != "" {
		t.Errorf("Model = %q, want empty", result.Model)
	}
	if api.dispatched.Nonce != "" {
		t.Errorf("Dispatch was called with %+v, want no dispatch", api.dispatched)
	}
}

func TestStartDocsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	_, err := newRunner(&fakeAPI{err: wantErr}).Start(t.Context(), review.Request{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Start() = %v, want wrapping %v", err, wantErr)
	}
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseClone {
		t.Errorf("Start() = %v, want a FailedError with cause %q", err, review.CauseClone)
	}
}

func TestStartUnparsableBaseDocsIsNotCloneFailure(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{docsFS: unreadableFS{fstest.MapFS{"docs/a.md": coverDoc("[main.go]")}}}
	_, err := newRunner(api).Start(t.Context(), review.Request{BaseSHA: "base", ChangedFiles: []review.ChangedFile{{Path: "x.go"}}})
	if err == nil {
		t.Fatal("Start() = nil, want an error")
	}
	var failed *review.FailedError
	if errors.As(err, &failed) {
		t.Errorf("Start() = %v, want a plain error, not FailedError %q", err, failed.Cause)
	}
}

func TestStartDispatchError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	runner := newRunner(&fakeAPI{err: wantErr})

	if _, err := runner.Start(t.Context(), review.Request{}); !errors.Is(err, wantErr) {
		t.Fatalf("Start() = %v, want wrapping %v", err, wantErr)
	}
}

func artifact(t *testing.T, head, nonce string, claude map[string]any) []byte {
	t.Helper()

	// Ordinary review fixtures include the static schema's required reason,
	// including when proposals are present. Schema rejection tests build raw artifacts.
	if out, ok := claude["structured_output"].(map[string]any); ok {
		if _, proposals := out["proposals"]; proposals {
			if _, reason := out["no_impact_reason"]; !reason {
				out["no_impact_reason"] = ""
			}
		}
	}
	b, err := json.Marshal(map[string]any{"head_sha": head, "nonce": nonce, "claude": claude})
	if err != nil {
		t.Fatalf("marshal artifact: %v", err)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

func validProposal() map[string]any {
	return map[string]any{
		"doc_path": "docs/a.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 3},
		"reason": "flag renamed", "content": "new text",
	}
}

const usageDoc = "---\ntitle: A\nsummary: S.\ncovers:\n  - main.go\n---\n# A\n\n## Usage\nold usage\n"

func TestCollect(t *testing.T) {
	t.Parallel()

	completion := review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", RunID: 99, Nonce: "n1"}
	proposal := validProposal()
	outside := map[string]any{
		"doc_path": "README.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 3},
		"reason": "x", "content": "y",
	}

	farAnchor := map[string]any{
		"doc_path": "docs/a.md", "section": "Usage", "anchor": map[string]any{"file": "main.go", "line": 50},
		"reason": "x", "content": "y",
	}

	unchangedAnchor := map[string]any{
		"doc_path": "docs/a.md", "section": "Usage", "anchor": map[string]any{"file": "other.go", "line": 3},
		"reason": "x", "content": "y",
	}

	wrongType := validProposal()
	wrongType["anchor"] = map[string]any{"line": "three", "file": "main.go"}
	missingContent := validProposal()
	delete(missingContent, "content")

	tests := []struct {
		name        string
		raw         []byte
		want        review.Result
		wantInvalid bool
	}{
		{
			name: "no impact",
			raw: artifact(t, "abc", "n1", map[string]any{
				"modelUsage":        map[string]any{"claude-sonnet-4-5": map[string]any{}},
				"structured_output": map[string]any{"no_impact_reason": "internal refactor", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-sonnet-4-5", Verdict: review.NoImpact{Reason: "internal refactor"}},
		},
		{
			name: "proposals",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{proposal}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.Proposals{{
				DocPath: "docs/a.md", Section: "Usage", Anchor: review.Anchor{File: "main.go", Line: 3},
				Reason: "flag renamed", Content: "new text",
				Original: "## Usage\nold usage\n", Lines: review.LineRange{Start: 9, End: 10},
			}}},
		},
		{
			name: "usage and cost",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd": 0.056967,
				"usage": map[string]any{
					"input_tokens": 4, "output_tokens": 966, "cache_read_input_tokens": 6475, "cache_creation_input_tokens": 11501,
					"iterations": []any{map[string]any{"input_tokens": 2, "output_tokens": 637}},
				},
				"modelUsage":        map[string]any{"claude-sonnet-5-5": map[string]any{"costBasis": "list"}, "claude-haiku": map[string]any{"costBasis": "list"}},
				"structured_output": map[string]any{"no_impact_reason": "internal refactor", "proposals": []any{}},
			}),
			want: review.Result{
				Model: "claude-haiku", Verdict: review.NoImpact{Reason: "internal refactor"},
				Usage: &review.Usage{Tokens: &review.Tokens{Input: 4, Output: 966, CacheRead: 6475, CacheWrite: 11501}, CostUSD: ptr(0.056967), CostBasis: "list"},
			},
		},
		{
			name: "usage without cost and with differing bases",
			raw: artifact(t, "abc", "n1", map[string]any{
				"usage":             map[string]any{"input_tokens": 1, "output_tokens": 2},
				"modelUsage":        map[string]any{"a": map[string]any{"costBasis": "list"}, "b": map[string]any{}},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "a", Verdict: review.NoImpact{Reason: "x"}, Usage: &review.Usage{Tokens: &review.Tokens{Input: 1, Output: 2}}},
		},
		{
			name: "modelUsage entry not an object",
			raw: artifact(t, "abc", "n1", map[string]any{
				"modelUsage":        map[string]any{"m": "n/a"},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "m", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "modelUsage not an object",
			raw: artifact(t, "abc", "n1", map[string]any{
				"modelUsage":        "n/a",
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "costBasis not a string",
			raw: artifact(t, "abc", "n1", map[string]any{
				"usage":             map[string]any{"input_tokens": 1},
				"modelUsage":        map[string]any{"m": map[string]any{"costBasis": map[string]any{}}},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "m", Verdict: review.NoImpact{Reason: "x"}, Usage: &review.Usage{Tokens: &review.Tokens{Input: 1}}},
		},
		{
			name: "token count not a number",
			raw: artifact(t, "abc", "n1", map[string]any{
				"usage":             map[string]any{"input_tokens": "12"},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "one malformed token count drops every count",
			raw: artifact(t, "abc", "n1", map[string]any{
				"usage":             map[string]any{"input_tokens": "12", "output_tokens": 5},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "cost not a number",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    "free",
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "cost without a usage block",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    0.5,
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}, Usage: &review.Usage{CostUSD: ptr(0.5)}},
		},
		{
			name: "null cost and a null token count are not reported",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    nil,
				"usage":             map[string]any{"input_tokens": nil, "output_tokens": 3},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "all null usage is nil",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    nil,
				"usage":             map[string]any{"input_tokens": nil},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "negative tokens and cost",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    -1,
				"usage":             map[string]any{"input_tokens": -5, "output_tokens": 7},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "absurd tokens and cost",
			raw: artifact(t, "abc", "n1", map[string]any{
				"total_cost_usd":    2e6,
				"usage":             map[string]any{"input_tokens": 2e12, "output_tokens": 7},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "x"}},
		},
		{
			name: "model name and cost basis over their caps",
			raw: artifact(t, "abc", "n1", map[string]any{
				"usage":             map[string]any{"input_tokens": 1},
				"modelUsage":        map[string]any{strings.Repeat("m", 300): map[string]any{"costBasis": strings.Repeat("b", 33)}},
				"structured_output": map[string]any{"no_impact_reason": "x", "proposals": []any{}},
			}),
			want: review.Result{Model: strings.Repeat("m", 200), Verdict: review.NoImpact{Reason: "x"}, Usage: &review.Usage{Tokens: &review.Tokens{Input: 1}}},
		},
		{name: "head mismatch", raw: artifact(t, "other", "n1", map[string]any{"structured_output": map[string]any{}}), wantInvalid: true},
		{name: "nonce mismatch", raw: artifact(t, "abc", "stale", map[string]any{"structured_output": map[string]any{}}), wantInvalid: true},
		{name: "claude error", raw: artifact(t, "abc", "n1", map[string]any{"is_error": true, "result": "401"}), wantInvalid: true},
		{name: "no proposals and no reason", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "", "proposals": []any{}}}), wantInvalid: true},
		{name: "no proposals and a blank reason", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "  \n", "proposals": []any{}}}), wantInvalid: true},
		{name: "anchor line not an integer", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{wrongType}}}), wantInvalid: true},
		{name: "proposal missing content", raw: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{missingContent}}}), wantInvalid: true},
		{name: "missing structured output", raw: artifact(t, "abc", "n1", map[string]any{}), wantInvalid: true},
		{name: "bad json", raw: []byte("{"), wantInvalid: true},
		{
			name: "anchor outside the hunks of a changed file",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{farAnchor}},
			}),
			wantInvalid: true,
		},
		{
			name: "no impact reason of several lines is stored as one",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"no_impact_reason": "  internal\n\nrefactor  \n", "proposals": []any{}},
			}),
			want: review.Result{Model: "claude-code", Verdict: review.NoImpact{Reason: "internal refactor"}},
		},
		{
			name: "anchor on a file the PR did not change",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{unchangedAnchor}},
			}),
			wantInvalid: true,
		},
		{
			name: "proposal outside docs",
			raw: artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{outside}},
			}),
			wantInvalid: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{
				artifact: tc.raw,
				changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
				files:    map[string][]byte{"docs/a.md": []byte(usageDoc)},
			}
			runner := newRunner(api)
			got, err := runner.Collect(t.Context(), completion)

			var invalid *review.InvalidResultError
			if errors.As(err, &invalid) != tc.wantInvalid || (err != nil) != tc.wantInvalid {
				t.Fatalf("Collect() error = %v, want InvalidResultError = %v", err, tc.wantInvalid)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("Collect() (-want +got):\n%s", diff)
			}
		})
	}
}

func TestCollectNewDoc(t *testing.T) {
	t.Parallel()

	newDoc := func(covers string) map[string]any {
		return map[string]any{
			"doc_path": "docs/new.md", "section": "", "anchor": map[string]any{"file": "main.go", "line": 3},
			"reason": "new feature", "content": "---\ntitle: T\nsummary: S\ncovers: " + covers + "\n---\n# T\n",
			"index_entry": "New feature",
		}
	}
	changed := []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}
	base := map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[other.go]")}}

	tests := []struct {
		name        string
		covers      string
		baseSHA     string
		wantInvalid bool
	}{
		{name: "covers an uncovered file", covers: "[main.go]", baseSHA: "base"},
		{name: "covers nothing uncovered", covers: "[other.go]", baseSHA: "base", wantInvalid: true},
		{name: "no stored base refuses a new doc", covers: "[main.go]", baseSHA: "", wantInvalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			raw := artifact(t, "abc", "n1", map[string]any{
				"structured_output": map[string]any{"proposals": []any{newDoc(tc.covers)}},
			})
			api := &fakeAPI{artifact: raw, changed: changed, docsAt: base}
			c := review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", BaseSHA: tc.baseSHA, RunID: 99, Nonce: "n1"}

			got, err := newRunner(api).Collect(t.Context(), c)

			var invalid *review.InvalidResultError
			if errors.As(err, &invalid) != tc.wantInvalid || (err != nil) != tc.wantInvalid {
				t.Fatalf("Collect() error = %v, want InvalidResultError = %v", err, tc.wantInvalid)
			}
			if _, ok := got.Verdict.(review.Proposals); ok == tc.wantInvalid {
				t.Errorf("Collect() verdict = %#v, want proposals = %v", got.Verdict, !tc.wantInvalid)
			}
		})
	}
}

func TestCollectArtifactError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	runner := newRunner(&fakeAPI{err: wantErr})

	_, err := runner.Collect(t.Context(), review.Completion{})
	var invalid *review.InvalidResultError
	if !errors.Is(err, wantErr) || errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want transient error wrapping %v", err, wantErr)
	}
}

func TestCollectChangedFilesError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("boom")
	raw := artifact(t, "abc", "n1", map[string]any{
		"structured_output": map[string]any{"proposals": []any{validProposal()}},
	})
	runner := newRunner(&fakeAPI{artifact: raw, changedErr: wantErr})

	_, err := runner.Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	var invalid *review.InvalidResultError
	if !errors.Is(err, wantErr) || errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want transient error wrapping %v", err, wantErr)
	}
}

func TestCollectErrorDoesNotEchoResult(t *testing.T) {
	t.Parallel()

	const injected = "SECRET-EXFIL"
	raw := artifact(t, "abc", "n1", map[string]any{
		"is_error": true, "result": injected, "subtype": "success",
		"terminal_reason": "api_error", "api_error_status": 401,
	})
	_, err := newRunner(&fakeAPI{artifact: raw}).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})

	var invalid *review.InvalidResultError
	if !errors.As(err, &invalid) {
		t.Fatalf("Collect() = %v, want *review.InvalidResultError", err)
	}
	if strings.Contains(err.Error(), injected) {
		t.Errorf("Collect() error %q contains the result text", err)
	}
	if want := "api_error_status 401 (terminal_reason api_error"; !strings.Contains(err.Error(), want) {
		t.Errorf("Collect() error %q, want it to contain %q", err, want)
	}
}

func TestCollectCapsProposalErrorText(t *testing.T) {
	t.Parallel()

	long := validProposal()
	long["doc_path"] = strings.Repeat("x", 5000)
	raw := artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{long}}})
	api := &fakeAPI{artifact: raw, changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}}

	_, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	if err == nil || len(err.Error()) > 1100 {
		t.Fatalf("Collect() error = %v (len %d), want a non-nil error under 1100 bytes", err, len(fmt.Sprint(err)))
	}
}

func TestCollectRetainsValidProposalAndNamesEveryDrop(t *testing.T) {
	t.Parallel()

	missingSection := validProposal()
	missingSection["section"] = "Nope"
	missingDoc := validProposal()
	missingDoc["doc_path"] = "docs/gone.md"
	raw := artifact(t, "abc", "n1", map[string]any{
		"structured_output": map[string]any{"proposals": []any{validProposal(), missingSection, missingDoc}},
	})
	api := &fakeAPI{
		artifact: raw,
		changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
		files:    map[string][]byte{"docs/a.md": []byte(usageDoc)},
	}

	got, err := newRunner(api).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1"})

	if err != nil {
		t.Fatal(err)
	}
	proposals, ok := got.Verdict.(review.Proposals)
	if !ok {
		t.Fatalf("verdict = %T, want proposals", got.Verdict)
	}
	if len(proposals) != 1 || proposals[0].Section != "Usage" || proposals[0].Original != "## Usage\nold usage\n" {
		t.Fatalf("retained proposals = %+v", proposals)
	}
	if len(got.Dropped) != 2 || got.Dropped[0].Index != 1 || got.Dropped[1].Index != 2 {
		t.Fatalf("drops = %+v", got.Dropped)
	}
	for _, d := range got.Dropped {
		if d.Reason == "" || len(d.Reason) > 203 || strings.ContainsAny(d.Reason, "\r\n") {
			t.Errorf("unbounded drop = %+v", d)
		}
	}
}

func TestCollectFillsOriginalAndLines(t *testing.T) {
	t.Parallel()

	const doc = "---\ntitle: A\nsummary: S.\ncovers:\n  - main.go\n---\n# A\n\n## Usage\nold usage\n\n## Other\nbody\n"
	const brokenFrontmatter = "---\ntitle: [unclosed\n---\n# B\n\n## Part\ntext\n"
	usage := validProposal()
	other := validProposal()
	other["section"] = "## Other"
	broken := validProposal()
	broken["doc_path"] = "docs/b.md"
	broken["section"] = "Part"

	raw := artifact(t, "abc", "n1", map[string]any{
		"structured_output": map[string]any{"proposals": []any{usage, other, broken}},
	})
	api := &fakeAPI{
		artifact: raw,
		changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
		files:    map[string][]byte{"docs/a.md": []byte(doc), "docs/b.md": []byte(brokenFrontmatter)},
	}

	got, err := newRunner(api).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1"})
	if err != nil {
		t.Fatalf("Collect() = %v, want nil", err)
	}

	proposals, ok := got.Verdict.(review.Proposals)
	if !ok || len(proposals) != 3 {
		t.Fatalf("Verdict = %#v, want 3 proposals", got.Verdict)
	}
	if p := proposals[0]; p.Original != "## Usage\nold usage\n\n" || p.Lines != (review.LineRange{Start: 9, End: 11}) {
		t.Errorf("usage Original, Lines = %q, %+v", p.Original, p.Lines)
	}
	if p := proposals[1]; p.Section != "Other" || p.Original != "## Other\nbody\n" || p.Lines != (review.LineRange{Start: 12, End: 13}) {
		t.Errorf("other Section, Original, Lines = %q, %q, %+v", p.Section, p.Original, p.Lines)
	}
	if p := proposals[2]; p.Original == "" || p.Lines == (review.LineRange{}) {
		t.Errorf("broken-frontmatter doc Original, Lines = %q, %+v, want them filled", p.Original, p.Lines)
	}
	if diff := cmp.Diff(map[string]int{"docs/a.md@abc": 1, "docs/b.md@abc": 1}, api.fileReads); diff != "" {
		t.Errorf("file reads (-want +got):\n%s", diff)
	}
}

func TestCollectRejectsMissingSectionOrDoc(t *testing.T) {
	t.Parallel()

	missingSection := validProposal()
	missingSection["section"] = "Nope"
	missingDoc := validProposal()
	missingDoc["doc_path"] = "docs/gone.md"

	for name, p := range map[string]map[string]any{"missing section": missingSection, "missing doc": missingDoc} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw := artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{p}}})
			api := &fakeAPI{
				artifact: raw,
				changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
				files:    map[string][]byte{"docs/a.md": []byte(usageDoc)},
			}

			_, err := newRunner(api).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1"})

			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) {
				t.Fatalf("Collect() = %v, want *review.InvalidResultError", err)
			}
		})
	}
}

const scaffoldFrontmatter = "---\ntitle: T\nsummary: S\ncovers: []\n---\n"

func validScaffoldOutput() map[string]any {
	return map[string]any{
		"index":        scaffoldFrontmatter + "[a](architecture.md) [s](guides/setup.md)\n",
		"architecture": scaffoldFrontmatter,
		"setup":        scaffoldFrontmatter,
	}
}

func TestStartScaffold(t *testing.T) {
	t.Parallel()

	api := &fakeAPI{runID: 42}
	runner := actions.New(api, 5*time.Minute, 10*time.Minute)

	before := time.Now()
	started, err := runner.StartScaffold(t.Context(), review.ScaffoldRequest{InstallationID: 1, Owner: "o", Repo: "r", BaseSHA: "base"})
	if err != nil {
		t.Fatalf("StartScaffold() = %v, want nil", err)
	}
	pending, ok := started.(review.Pending)
	if !ok {
		t.Fatalf("StartScaffold() = %T, want review.Pending", started)
	}
	if pending.RunID != 42 || pending.Nonce == "" {
		t.Errorf("Pending = %+v, want run 42 and a nonce", pending)
	}
	if pending.Deadline.Before(before.Add(10*time.Minute)) || pending.Deadline.After(time.Now().Add(10*time.Minute)) {
		t.Errorf("StartScaffold() deadline = %v, want the 10m scaffold timeout from %v", pending.Deadline, before)
	}
	want := actions.DispatchInputs{HeadSHA: "base", PRNumber: 0, Nonce: pending.Nonce, Input: input.New(review.Request{}, basedocs.Selection{})}
	if diff := cmp.Diff(want, api.dispatched); diff != "" {
		t.Errorf("dispatch inputs (-want +got):\n%s", diff)
	}
}

func TestCollectScaffold(t *testing.T) {
	t.Parallel()

	completion := review.Completion{Owner: "o", Repo: "r", HeadSHA: "base", RunID: 42, Nonce: "n1"}
	badIndex := validScaffoldOutput()
	badIndex["index"] = scaffoldFrontmatter

	tests := []struct {
		name        string
		art         []byte
		wantInvalid string
	}{
		{name: "valid", art: artifact(t, "base", "n1", map[string]any{"structured_output": validScaffoldOutput(), "modelUsage": map[string]any{"m1": map[string]any{}}})},
		{name: "mismatched nonce", art: artifact(t, "base", "other", map[string]any{"structured_output": validScaffoldOutput()}), wantInvalid: "nonce"},
		{name: "mismatched head", art: artifact(t, "other", "n1", map[string]any{"structured_output": validScaffoldOutput()}), wantInvalid: "head_sha"},
		{name: "invalid docs", art: artifact(t, "base", "n1", map[string]any{"structured_output": badIndex}), wantInvalid: "docs/README.md"},
		{name: "is_error", art: artifact(t, "base", "n1", map[string]any{"is_error": true, "structured_output": validScaffoldOutput()}), wantInvalid: "claude code failed"},
		{name: "missing output", art: artifact(t, "base", "n1", map[string]any{}), wantInvalid: "no structured_output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			runner := newRunner(&fakeAPI{artifact: tc.art})
			got, err := runner.CollectScaffold(t.Context(), completion)
			if tc.wantInvalid == "" {
				if err != nil {
					t.Fatalf("CollectScaffold() = %v, want nil", err)
				}
				want := review.Scaffold{
					Runner:       "actions",
					Model:        "m1",
					Index:        scaffoldFrontmatter + "[a](architecture.md) [s](guides/setup.md)\n",
					Architecture: scaffoldFrontmatter,
					Setup:        scaffoldFrontmatter,
				}
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("Scaffold (-want +got):\n%s", diff)
				}
				return
			}

			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), tc.wantInvalid) {
				t.Fatalf("CollectScaffold() = %v, want *InvalidResultError containing %q", err, tc.wantInvalid)
			}
		})
	}
}

func TestCollectNewDocAlreadyAtHead(t *testing.T) {
	t.Parallel()

	proposal := map[string]any{
		"doc_path": "docs/new.md", "section": "", "anchor": map[string]any{"file": "main.go", "line": 3},
		"reason": "new feature", "content": "---\ntitle: T\nsummary: S\ncovers: [main.go]\n---\n# T\n",
		"index_entry": "New feature",
	}
	raw := artifact(t, "abc", "n1", map[string]any{
		"structured_output": map[string]any{"proposals": []any{proposal}},
	})

	tests := []struct {
		name  string
		files map[string][]byte
		paths map[string]bool
		dirs  map[string]bool
	}{
		{name: "file", files: map[string][]byte{"docs/new.md": []byte("# existing\n")}},
		{name: "directory or oversized file", paths: map[string]bool{"docs/new.md": true}, dirs: map[string]bool{"docs": true}},
		{name: "parent is a file", paths: map[string]bool{"docs": true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			api := &fakeAPI{
				artifact: raw,
				changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
				files:    tc.files,
				paths:    tc.paths,
				dirs:     tc.dirs,
				docsAt:   map[string]fstest.MapFS{"base": {"docs/a.md": coverDoc("[other.go]")}},
			}
			c := review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", BaseSHA: "base", RunID: 99, Nonce: "n1"}

			_, err := newRunner(api).Collect(t.Context(), c)

			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "already exists at head") {
				t.Fatalf("Collect() error = %v, want InvalidResultError naming an existing new doc", err)
			}
		})
	}
}

func TestActionKeepsClaudeToolsReadOnly(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../../../action/run-claude.sh")
	if err != nil {
		t.Fatalf("read run-claude.sh: %v", err)
	}
	action := string(raw)

	if !strings.Contains(action, "--tools Read,Grep,Glob ") {
		t.Error("run-claude.sh must allow exactly --tools Read,Grep,Glob")
	}
	_, after, found := strings.Cut(action, "--disallowedTools ")
	if !found {
		t.Fatal("run-claude.sh has no --disallowedTools list")
	}
	list := strings.Fields(after)[0]
	for _, tool := range []string{"Bash", "Edit", "Write", "WebFetch", "WebSearch"} {
		if !strings.Contains(","+list+",", ","+tool+",") {
			t.Errorf("--disallowedTools %s is missing %s", list, tool)
		}
	}
}

func TestActionRejectsNonNumericPRNumber(t *testing.T) {
	t.Parallel()

	stubDir, runnerTemp := t.TempDir(), t.TempDir()
	marker := filepath.Join(stubDir, "ran")
	stub := "#!/bin/sh\ntouch " + marker + "\n"
	writeStubClaude(t, stubDir, stub)

	cmd := exec.CommandContext(t.Context(), "bash", "../../../../action/run-claude.sh")
	cmd.Env = []string{
		"PATH=" + stubDir + ":/usr/bin:/bin", "RUNNER_TEMP=" + runnerTemp, "PR_NUMBER=1; x",
		"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=", "ACTION_PATH=.", "CHECKOUT=.", "HEAD_SHA=abc",
		"DEFAULT_BRANCH=main", "NONCE=n", "DOCS=[]",
	}
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Errorf("run-claude.sh with PR_NUMBER=\"1; x\" succeeded, want a non-zero exit; output: %s", out)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stub claude ran (stat error = %v), want it never started", err)
	}
}

const fixtureHunks = "@@ -1,3 +1,4 @@ func main\n package main\n-var a = 1\n+var a = 2\n+var b = 3\n \n@@ -20,2 +21,3 @@\n \tx()\n+\ty()\n \\ No newline at end of file\n"

const fixtureDiff = "diff --git a/main.go b/main.go\nindex 1111111..2222222 100644\n--- a/main.go\n+++ b/main.go\n" + fixtureHunks

// scenarioScript builds the repository the diff-step tests run in: main has base.txt (20 lines)
// and origin/main points at it; branch feature adds f.txt; branch pr, off feature, edits lines 2
// and 15 of base.txt. It prints the merge base of feature and pr, then pr's head.
const scenarioScript = `set -eu
git init -q -b main "$REPO"
cd "$REPO"
git config user.email t@example.com
git config user.name t
seq 1 20 > base.txt
git add base.txt
git commit -qm base
git update-ref refs/remotes/origin/main HEAD
git checkout -qb feature
echo feature > f.txt
git add f.txt
git commit -qm F
git checkout -qb pr
sed 's/^2$/two/;s/^15$/fifteen/' base.txt > base.new
mv base.new base.txt
git commit -qam P
git merge-base feature pr
git rev-parse HEAD
`

type scenario struct{ repo, mergeBase, git string }

func newScenario(t *testing.T) scenario {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	cmd := exec.CommandContext(t.Context(), "bash", "-c", scenarioScript)
	cmd.Env = []string{"PATH=" + filepath.Dir(git) + ":/usr/bin:/bin", "HOME=" + t.TempDir(), "REPO=" + repo}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("build the scenario repository: %v", err)
	}
	shas := strings.Fields(string(out))
	if len(shas) != 2 {
		t.Fatalf("scenario script printed %q, want the merge base and the head", out)
	}
	return scenario{repo: repo, mergeBase: shas[0], git: git}
}

type anchorSpan struct {
	File     string
	Min, Max int
}

func anchorSpans(t *testing.T, raw []byte) []anchorSpan {
	t.Helper()
	var schema struct {
		Properties struct {
			Proposals struct {
				Items struct {
					Properties struct {
						Anchor struct {
							AnyOf []struct {
								Properties struct {
									File struct {
										Const string `json:"const"`
									} `json:"file"`
									Line struct {
										AnyOf []struct {
											Minimum int `json:"minimum"`
											Maximum int `json:"maximum"`
										} `json:"anyOf"`
									} `json:"line"`
								} `json:"properties"`
							} `json:"anyOf"`
						} `json:"anchor"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"proposals"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode --json-schema %s: %v", raw, err)
	}
	var got []anchorSpan
	for _, a := range schema.Properties.Proposals.Items.Properties.Anchor.AnyOf {
		for _, r := range a.Properties.Line.AnyOf {
			got = append(got, anchorSpan{a.Properties.File.Const, r.Minimum, r.Maximum})
		}
	}
	return got
}

func readOut(t *testing.T, out, name string) string {
	t.Helper()
	raw, err := fs.ReadFile(os.DirFS(out), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

func TestActionDiffsFromBaseSHA(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	docs := `{"base_sha":"` + sc.mergeBase + `","review":[],"uncovered":[]}`
	_, out := runClaudeWithStub(t, sc, docs)

	for _, name := range []string{"pr.diff", "pr.numbered.diff"} {
		got := readOut(t, out, name)
		if !strings.Contains(got, "+fifteen") || strings.Contains(got, "f.txt") {
			t.Errorf("%s = %q, want P's change and not F's", name, got)
		}
	}
}

// Without base_sha the action falls back to origin/<default>...HEAD.
func TestActionWithoutBaseSHADiffsFromDefaultBranch(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	_, out := runClaudeWithStub(t, sc, `{"review":[],"uncovered":[]}`)

	got := readOut(t, out, "pr.diff")
	if !strings.Contains(got, "+fifteen") || !strings.Contains(got, "f.txt") {
		t.Errorf("pr.diff = %q, want origin/main...HEAD with both P's and F's changes", got)
	}
}

func TestActionRejectsMalformedBaseSHA(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	for _, docs := range []string{`{"base_sha":"--output=x"}`, `{"base_sha":"ABCDEF"}`, `{"base_sha":1}`,
		`{"files":"x"}`, `{"files":[{"path":"a.go","ranges":[{"start":"1","end":2}]}]}`} {
		if _, _, err := runClaude(t, sc, docs); err == nil {
			t.Errorf("run-claude.sh with DOCS=%s succeeded, want a non-zero exit", docs)
		}
	}
}

func TestActionAnchorsOnTheDiffWithoutFiles(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	raw, _ := runClaudeWithStub(t, sc, `{"base_sha":"`+sc.mergeBase+`","review":[],"uncovered":[]}`)

	want := []anchorSpan{{"base.txt", 1, 5}, {"base.txt", 12, 18}}
	if diff := cmp.Diff(want, anchorSpans(t, raw)); diff != "" {
		t.Errorf("anchor anyOf (-want +got):\n%s", diff)
	}
}

func TestActionAnchorsOnTheGivenFiles(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	docs := `{"base_sha":"` + sc.mergeBase + `","review":[],"uncovered":[],"files":[` +
		`{"path":"base.txt","ranges":[{"start":3,"end":4}]},` +
		`{"path":"other.go","ranges":[{"start":10,"end":12},{"start":20,"end":20}]},` +
		`{"path":"old.go","ranges":[]}]}`
	raw, _ := runClaudeWithStub(t, sc, docs)

	want := []anchorSpan{{"base.txt", 3, 4}, {"other.go", 10, 12}, {"other.go", 20, 20}}
	if diff := cmp.Diff(want, anchorSpans(t, raw)); diff != "" {
		t.Errorf("anchor anyOf (-want +got):\n%s", diff)
	}
}

func TestActionAcceptsOldInputShapes(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	for name, docs := range map[string]string{
		"bare array":                   `["docs/a.md"]`,
		"object without base or files": `{"review":["docs/a.md"],"uncovered":["src/x.go"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, _ := runClaudeWithStub(t, sc, docs)
			want := []anchorSpan{{"base.txt", 1, 5}, {"base.txt", 12, 18}, {"f.txt", 1, 1}}
			if diff := cmp.Diff(want, anchorSpans(t, raw)); diff != "" {
				t.Errorf("anchor anyOf (-want +got):\n%s", diff)
			}
		})
	}
}

func TestActionNumbersDiff(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	_, out := runClaudeWithStub(t, sc, `{"base_sha":"`+sc.mergeBase+`"}`)

	diffText := readOut(t, out, "pr.diff")
	header, hunks, _ := strings.Cut(diffText, "@@")
	if want := header + review.NumberedPatch("@@"+hunks); readOut(t, out, "pr.numbered.diff") != want {
		t.Errorf("pr.numbered.diff does not match review.NumberedPatch over pr.diff")
	}
}

const blankLineDiff = "diff --git a/main.go b/main.go\n--- a/main.go\n+++ b/main.go\n@@ -1,4 +1,5 @@\n package main\n\n-var a = 1\n+var a = 2\n+var b = 3\n x\n"

func TestNumberedDiffAwkMatchesNumberedPatch(t *testing.T) {
	t.Parallel()

	for name, diffText := range map[string]string{"fixture": fixtureDiff, "empty line in hunk": blankLineDiff} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), "awk", "-f", "../../../../action/numbered-diff.awk")
			cmd.Stdin = strings.NewReader(diffText)
			got, err := cmd.Output()
			if err != nil {
				t.Fatalf("awk numbered-diff.awk = %v", err)
			}
			_, hunks, _ := strings.Cut(string(got), "@@")
			_, patch, _ := strings.Cut(diffText, "@@")
			if diff := cmp.Diff(review.NumberedPatch("@@"+patch), "@@"+hunks); diff != "" {
				t.Errorf("awk output differs from review.NumberedPatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestDiffHunksAwkPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		diff string
		want string
	}{
		{"raw UTF-8 path", "--- a/docs/über.md\n+++ b/docs/über.md\n@@ -1 +1,2 @@\n x\n+y\n", `{"file":"docs/über.md","start":1,"end":2}` + "\n"},
		{"path with a space", "--- a/docs/a b.md\n+++ b/docs/a b.md\t\n@@ -1 +1,2 @@\n x\n+y\n", `{"file":"docs/a b.md","start":1,"end":2}` + "\n"},
		{
			"quoted path",
			"--- a/x\n+++ \"b/docs/a\\\"b\\\\c\\td\\001.md\"\n@@ -1 +1,2 @@\n x\n+y\n",
			`{"file":"docs/a\"b\\c\u0009d\u0001.md","start":1,"end":2}` + "\n",
		},
		{"deleted file", "--- a/x.md\n+++ /dev/null\n@@ -1 +0,0 @@\n-x\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), "awk", "-f", "../../../../action/diff-hunks.awk")
			cmd.Stdin = strings.NewReader(tc.diff)
			got, err := cmd.Output()
			if err != nil {
				t.Fatalf("awk diff-hunks.awk = %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("diff-hunks.awk = %q, want %q", got, tc.want)
			}
		})
	}
}

// runClaude runs run-claude.sh for PR 7 in the scenario repository with the given docs input, with a stub claude
// that records the --json-schema it receives into <runner temp>/schema.json.
func runClaude(t *testing.T, sc scenario, docs string) (out, log string, err error) {
	t.Helper()
	jq, lookErr := exec.LookPath("jq")
	if lookErr != nil {
		t.Skip("jq is not installed")
	}
	action, absErr := filepath.Abs("../../../../action")
	if absErr != nil {
		t.Fatalf("resolve action dir: %v", absErr)
	}
	stubDir, runnerTemp := t.TempDir(), t.TempDir()
	out = filepath.Join(runnerTemp, "pollux-agent")
	schemaOut := filepath.Join(runnerTemp, "schema.json")
	stub := "#!/bin/sh\nwhile [ $# -gt 0 ]; do\n  if [ \"$1\" = --json-schema ]; then printf %s \"$2\" > " + schemaOut + "; fi\n  shift\ndone\n"
	writeStubClaude(t, stubDir, stub)
	cmd := exec.CommandContext(t.Context(), "bash", "../../../../action/run-claude.sh")
	cmd.Env = []string{
		"PATH=" + stubDir + ":" + filepath.Dir(jq) + ":" + filepath.Dir(sc.git) + ":/usr/bin:/bin", "RUNNER_TEMP=" + runnerTemp, "PR_NUMBER=7",
		"CLAUDE_CODE_OAUTH_TOKEN=", "ANTHROPIC_API_KEY=", "ACTION_PATH=" + action, "CHECKOUT=" + sc.repo, "HEAD_SHA=abc",
		"DEFAULT_BRANCH=main", "NONCE=n", "DOCS=" + docs,
	}
	combined, runErr := cmd.CombinedOutput()
	if runErr != nil {
		return out, string(combined), fmt.Errorf("run-claude.sh: %w, output: %s", runErr, combined)
	}
	return out, string(combined), nil
}

// runClaudeWithStub runs run-claude.sh for PR 7 and returns the --json-schema the stub claude
// received and the output directory.
func runClaudeWithStub(t *testing.T, sc scenario, docs string) (schema []byte, out string) {
	t.Helper()
	out, _, err := runClaude(t, sc, docs)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "result.json")); err != nil {
		t.Fatalf("result.json: %v", err)
	}
	return []byte(readOut(t, filepath.Dir(out), "schema.json")), out
}

// filesDocs is a docs input whose files give each of n files perFile one-line ranges.
func filesDocs(baseSHA string, n, perFile int) string {
	files := make([]string, n)
	for f := range n {
		ranges := make([]string, perFile)
		for h := range perFile {
			ranges[h] = fmt.Sprintf(`{"start":%d,"end":%d}`, 2*h+2, 2*h+2)
		}
		files[f] = fmt.Sprintf(`{"path":"a%d.go","ranges":[%s]}`, f, strings.Join(ranges, ","))
	}
	return `{"base_sha":"` + baseSHA + `","files":[` + strings.Join(files, ",") + `]}`
}

func TestActionGroupsAnchorsByFile(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	raw, _ := runClaudeWithStub(t, sc, filesDocs(sc.mergeBase, 2, 150))
	if len(raw) >= 100*1024 {
		t.Errorf("schema is %d bytes, want under 100 KiB", len(raw))
	}
	var schema struct {
		Properties struct {
			Proposals struct {
				Items struct {
					Properties struct {
						Anchor struct {
							AnyOf []struct {
								Properties struct {
									File struct {
										Const string `json:"const"`
									} `json:"file"`
									Line struct {
										AnyOf []struct {
											Minimum int `json:"minimum"`
											Maximum int `json:"maximum"`
										} `json:"anyOf"`
									} `json:"line"`
								} `json:"properties"`
							} `json:"anyOf"`
						} `json:"anchor"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"proposals"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode --json-schema: %v", err)
	}
	entries := schema.Properties.Proposals.Items.Properties.Anchor.AnyOf
	if len(entries) != 2 {
		t.Fatalf("anchor anyOf has %d entries, want 2 (one per file)", len(entries))
	}
	for i, e := range entries {
		if want := fmt.Sprintf("a%d.go", i); e.Properties.File.Const != want {
			t.Errorf("entry %d file = %q, want %q", i, e.Properties.File.Const, want)
		}
		lines := e.Properties.Line.AnyOf
		if len(lines) != 150 {
			t.Fatalf("entry %d has %d ranges, want 150", i, len(lines))
		}
		for h, r := range lines {
			if want := 2*h + 2; r.Minimum != want || r.Maximum != want {
				t.Fatalf("entry %d range %d = %d..%d, want %d..%d", i, h, r.Minimum, r.Maximum, want, want)
			}
		}
	}
}

func TestActionDropsAnchorRangesWhenTheSchemaIsTooLarge(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	docs := filesDocs(sc.mergeBase, 2, 1500)
	// Linux refuses to exec with one environment string over 128 KiB (MAX_ARG_STRLEN).
	if len(docs) >= 128<<10 {
		t.Fatalf("docs input is %d bytes, want under 128 KiB", len(docs))
	}
	out, log, err := runClaude(t, sc, docs)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(log, "::warning::") {
		t.Errorf("run-claude.sh output = %q, want a ::warning:: that the anchor ranges were dropped", log)
	}
	raw := []byte(readOut(t, filepath.Dir(out), "schema.json"))
	if len(raw) > 100<<10 || strings.Contains(string(raw), `"minimum"`) {
		t.Errorf("--json-schema = %d bytes, want the schema without per-file anchors", len(raw))
	}
	requireNewDocIndexEntry(t, raw)
	if _, err := os.Stat(filepath.Join(out, "result.json")); err != nil {
		t.Errorf("result.json: %v", err)
	}
}

// requireNewDocIndexEntry checks the schema carries review.Proposal.Validate's
// rule that index_entry is set iff section is empty.
func requireNewDocIndexEntry(t *testing.T, raw []byte) {
	t.Helper()
	var schema struct {
		Properties struct {
			Proposals struct {
				Items struct {
					If struct {
						Properties struct {
							Section struct {
								Const *string `json:"const"`
							} `json:"section"`
						} `json:"properties"`
					} `json:"if"`
					Then struct {
						Required   []string `json:"required"`
						Properties struct {
							DocPath struct {
								Pattern string `json:"pattern"`
							} `json:"doc_path"`
						} `json:"properties"`
					} `json:"then"`
					Else struct {
						Properties struct {
							IndexEntry struct {
								MaxLength *int `json:"maxLength"`
							} `json:"index_entry"`
						} `json:"properties"`
					} `json:"else"`
				} `json:"items"`
			} `json:"proposals"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("decode --json-schema: %v", err)
	}
	items := schema.Properties.Proposals.Items
	if p := items.Then.Properties.DocPath.Pattern; p != `^docs/.*\.md$` {
		t.Errorf("--json-schema new-doc doc_path pattern = %q, want .md files under docs/", p)
	}
	if c := items.If.Properties.Section.Const; c == nil || *c != "" || !slices.Contains(items.Then.Required, "index_entry") || items.Else.Properties.IndexEntry.MaxLength == nil || *items.Else.Properties.IndexEntry.MaxLength != 0 {
		t.Errorf("--json-schema proposal items = %+v, want index_entry required iff section is empty", items)
	}
}

func TestActionSchemaRequiresIndexEntryForNewDocs(t *testing.T) {
	t.Parallel()

	sc := newScenario(t)
	raw, _ := runClaudeWithStub(t, sc, filesDocs(sc.mergeBase, 1, 2))
	requireNewDocIndexEntry(t, raw)
}

// writeStubClaude writes an executable stand-in for the claude CLI into dir.
func writeStubClaude(t *testing.T, dir, script string) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open stub dir: %v", err)
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("claude", []byte(script), 0o700); err != nil {
		t.Fatalf("write stub claude: %v", err)
	}
}
