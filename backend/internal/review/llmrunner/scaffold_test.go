package llmrunner_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const scaffoldFrontmatter = "---\ntitle: T\nsummary: S\ncovers: []\n---\n"

func submitDocsCall(id, index string) llm.ToolCall {
	args, err := json.Marshal(map[string]string{"index": index, "architecture": scaffoldFrontmatter, "setup": scaffoldFrontmatter})
	if err != nil {
		panic(fmt.Sprintf("marshal submit_docs args: %v", err))
	}
	return llm.ToolCall{ID: id, Name: "submit_docs", Args: args}
}

func toolResponse(call llm.ToolCall) func(llm.Request) (llm.Response, error) {
	return func(llm.Request) (llm.Response, error) { return llm.Response{ToolCalls: []llm.ToolCall{call}}, nil }
}

func lastToolResult(req llm.Request) llm.ToolResult {
	last := req.Messages[len(req.Messages)-1]
	if len(last.ToolResults) != 1 {
		return llm.ToolResult{}
	}
	return last.ToolResults[0]
}

func startScaffold(t *testing.T, model llm.Model) (review.ScaffoldStarted, error) {
	t.Helper()
	repoDir, sha := newGitRepo(t)
	runner := newRunnerWith(model, "m", "m", slog.New(slog.DiscardHandler))
	runner.SetRemote(repoDir)
	started, err := runner.StartScaffold(t.Context(), review.ScaffoldRequest{Owner: "o", Repo: "r", BaseSHA: sha})
	if err != nil {
		return nil, fmt.Errorf("start scaffold: %w", err)
	}
	return started, nil
}

func TestStartScaffold_BrokenIndexIsReturnedToModel(t *testing.T) {
	t.Parallel()

	goodIndex := scaffoldFrontmatter + "[a](architecture.md) [s](guides/setup.md)\n"
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		toolResponse(submitDocsCall("1", scaffoldFrontmatter)),
		func(req llm.Request) (llm.Response, error) {
			if res := lastToolResult(req); !res.IsError || !strings.Contains(res.Content, "architecture.md") {
				t.Errorf("tool result = %+v, want an error naming the missing link", res)
			}
			return llm.Response{ToolCalls: []llm.ToolCall{submitDocsCall("2", goodIndex)}}, nil
		},
	}}

	started, err := startScaffold(t, model)
	if err != nil {
		t.Fatalf("StartScaffold() = %v, want nil", err)
	}
	want := review.Scaffold{Runner: "llmrunner", Model: "m", Index: goodIndex, Architecture: scaffoldFrontmatter, Setup: scaffoldFrontmatter}
	got, ok := started.(review.Scaffold)
	if !ok {
		t.Fatalf("StartScaffold() = %T, want review.Scaffold", started)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Scaffold (-want +got):\n%s", diff)
	}
}

func TestStartScaffold_ReadsOutsideTheCloneAreToolErrors(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"../outside", "/etc/passwd"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			args, err := json.Marshal(map[string]string{"path": path})
			if err != nil {
				t.Fatalf("marshal args: %v", err)
			}
			model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
				toolResponse(llm.ToolCall{ID: "r", Name: "read_file", Args: args}),
				func(req llm.Request) (llm.Response, error) {
					res := lastToolResult(req)
					if !res.IsError || strings.Contains(res.Content, "root:") {
						t.Errorf("tool result = %+v, want an error with no file content", res)
					}
					return llm.Response{ToolCalls: []llm.ToolCall{submitDocsCall("s", scaffoldFrontmatter+"[a](architecture.md) [s](guides/setup.md)\n")}}, nil
				},
			}}

			if _, err := startScaffold(t, model); err != nil {
				t.Fatalf("StartScaffold() = %v, want nil", err)
			}
			if got := len(model.calls); got != 2 {
				t.Fatalf("model calls = %d, want 2", got)
			}
			if prompt := model.calls[0].Messages[0].Text; !strings.Contains(prompt, "o/r") {
				t.Errorf("prompt = %q, want it to name the repo", prompt)
			}
		})
	}
}

func TestStartScaffold_StepLimitIsLimitFailure(t *testing.T) {
	t.Parallel()

	args := json.RawMessage(`{"path": "."}`)
	loop := toolResponse(llm.ToolCall{ID: "l", Name: "list_dir", Args: args})
	script := make([]func(llm.Request) (llm.Response, error), 100)
	for i := range script {
		script[i] = loop
	}

	_, err := startScaffold(t, &fakeModel{script: script})
	var failedErr *review.FailedError
	if !errors.As(err, &failedErr) || failedErr.Cause != review.CauseLimit || !errors.Is(err, agent.ErrStepLimit) {
		t.Fatalf("StartScaffold() = %v, want *FailedError with CauseLimit wrapping agent.ErrStepLimit", err)
	}
}
