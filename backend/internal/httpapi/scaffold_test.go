package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const (
	scaffoldIndex = "---\ntitle: Docs index\nsummary: Map of the docs.\ncovers: []\n---\n# Docs\n\n## Index\n\n" +
		"- [Architecture](architecture.md): how cmd/app is put together.\n- [Setup](guides/setup.md): build and test with make test.\n"
	scaffoldArchitecture = "---\ntitle: Architecture\nsummary: The parts of the app.\ncovers:\n  - \"cmd/**\"\n---\n# Architecture\n\ncmd/app is the entry point.\n"
	scaffoldSetup        = "---\ntitle: Setup\nsummary: Build and test.\ncovers:\n  - \"Makefile\"\n---\n# Setup\n\nRun `make test`.\n"
)

// scaffoldGitHub is a GitHub with a default branch at tip, no docs/ anywhere,
// and a record of every branch, commit and pull request the gate makes.
type scaffoldGitHub struct {
	noComments
	unusedCommentGitHub
	tip string

	created chan scaffoldCheckRun
	updated chan scaffoldCheckRun

	mu       sync.Mutex
	nextID   int64
	branches map[string]string
	creates  int
	commits  []scaffoldCommit
	prs      []gate.NewPullRequest
	resets   int
	existing *gate.ScaffoldPR // the pull request FindPullRequest reports, if any
}

type scaffoldCheckRun struct {
	id  int64
	run gate.CheckRun
}

type scaffoldCommit struct {
	branch string
	parent string
	files  map[string]string
}

func newScaffoldGitHub(tip string) *scaffoldGitHub {
	return &scaffoldGitHub{tip: tip, created: make(chan scaffoldCheckRun, 8), updated: make(chan scaffoldCheckRun, 8), nextID: 100, branches: map[string]string{}}
}

func (f *scaffoldGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func (f *scaffoldGitHub) DocsExist(context.Context, int64, string, string, string) (bool, error) {
	return false, nil
}

func (f *scaffoldGitHub) MergeBase(context.Context, int64, string, string, string, string) (string, error) {
	return "", fmt.Errorf("a PR without docs/ must not be analyzed")
}

func (f *scaffoldGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return nil, fmt.Errorf("a PR without docs/ must not be analyzed")
}

func (f *scaffoldGitHub) CreateCheckRun(_ context.Context, _ int64, _, _ string, run gate.CheckRun) (int64, error) {
	f.mu.Lock()
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	f.created <- scaffoldCheckRun{id: id, run: run}
	return id, nil
}

func (f *scaffoldGitHub) UpdateCheckRun(_ context.Context, _ int64, _, _ string, id int64, run gate.CheckRun) error {
	f.updated <- scaffoldCheckRun{id: id, run: run}
	return nil
}

func (f *scaffoldGitHub) DefaultBranch(context.Context, int64, string, string) (string, string, error) {
	return "main", f.tip, nil
}

func (f *scaffoldGitHub) CreateBranch(_ context.Context, _ int64, _, _, branch, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if _, ok := f.branches[branch]; ok {
		return gate.ErrBranchExists
	}
	f.branches[branch] = sha
	return nil
}

func (f *scaffoldGitHub) ResetBranch(_ context.Context, _ int64, _, _, branch, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resets++
	f.branches[branch] = sha
	return nil
}

func (f *scaffoldGitHub) BranchSHA(_ context.Context, _ int64, _, _, branch string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branches[branch], nil
}

func (f *scaffoldGitHub) CommitFiles(_ context.Context, _ int64, _, _, branch, parentSHA string, files []gate.FileChange, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.branches[branch] != parentSHA {
		return "", gate.ErrBranchMoved
	}
	commit := scaffoldCommit{branch: branch, parent: parentSHA, files: map[string]string{}}
	for _, file := range files {
		commit.files[file.Path] = file.Content
	}
	f.commits = append(f.commits, commit)
	f.branches[branch] = fmt.Sprintf("commit-%d", len(f.commits))
	return f.branches[branch], nil
}

func (f *scaffoldGitHub) CreatePullRequest(_ context.Context, _ int64, _, _ string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs = append(f.prs, pr)
	return gate.ScaffoldPR{Number: 9, URL: "https://github.com/acme/widgets/pull/9"}, nil
}

func (f *scaffoldGitHub) FindPullRequest(context.Context, int64, string, string, string) (gate.ScaffoldPR, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.existing == nil {
		return gate.ScaffoldPR{}, false, nil
	}
	return *f.existing, true, nil
}

// scaffoldModel lists the repo, submits docs the index of which lacks its links,
// then submits valid ones.
type scaffoldModel struct {
	requests []llm.Request
}

func (m *scaffoldModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	m.requests = append(m.requests, req)
	switch len(m.requests) {
	case 1:
		return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "list_dir", Args: json.RawMessage(`{"path":"."}`)}}}, nil
	case 2:
		return llm.Response{ToolCalls: []llm.ToolCall{submitDocs("1", "---\ntitle: Docs\nsummary: S\ncovers: []\n---\n# Docs\n", scaffoldArchitecture, scaffoldSetup)}}, nil
	case 3:
		return llm.Response{ToolCalls: []llm.ToolCall{submitDocs("2", scaffoldIndex, scaffoldArchitecture, scaffoldSetup)}}, nil
	default:
		return llm.Response{}, fmt.Errorf("unexpected model request %d", len(m.requests))
	}
}

func submitDocs(id, index, architecture, setup string) llm.ToolCall {
	args, err := json.Marshal(map[string]string{"index": index, "architecture": architecture, "setup": setup})
	if err != nil {
		panic(err)
	}
	return llm.ToolCall{ID: id, Name: "submit_docs", Args: args}
}

func waitCall(t *testing.T, calls chan scaffoldCheckRun) scaffoldCheckRun {
	t.Helper()

	select {
	case call := <-calls:
		return call
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a check run call")
		return scaffoldCheckRun{}
	}
}

func TestWebhookToScaffoldPullRequest(t *testing.T) {
	repoDir, tip := newGitRepo(t, map[string]string{
		"Makefile":         "test:\n\tgo test ./...\n",
		"cmd/app/main.go":  "package main\n\nfunc main() {}\n",
		"internal/doc.go":  "package internal\n",
		"README.md":        "# widgets\n",
		"assets/logo.txt":  "logo\n",
		"scripts/build.sh": "#!/bin/sh\n",
	})
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")

	secret := []byte("test-secret")
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := newScaffoldGitHub(tip)
	model := &scaffoldModel{}
	noToken := func(context.Context, int64, string) (string, error) { return "", nil }
	gateSvc := gate.NewService(gh, gh, store, gate.Runners{Server: newServerRunner(model, noToken)}, gh, httpapi.NewScaffoldQueue(store))
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), slog.New(slog.DiscardHandler), 8)
	stopWorker := runWorker(worker)
	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() { stopErr = stopWorker() })
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	handler := httpapi.NewHandler(httpapi.Deps{Logger: slog.New(slog.DiscardHandler), WebhookSecret: secret, Jobs: worker, Runs: store})

	if code := postSigned(t, handler, secret, "d1", e2ePullRequestBody(t, 1, tip)); code != http.StatusAccepted {
		t.Fatalf("first POST /webhook = %d, want %d", code, http.StatusAccepted)
	}
	first := waitCall(t, gh.created)
	if first.run.Conclusion != gate.ConclusionNeutral || first.run.Title != "No docs/ folder" || first.run.Name != "pollux-agent" {
		t.Errorf("first check run = %+v, want neutral \"No docs/ folder\" named pollux-agent", first.run)
	}
	linked := waitCall(t, gh.updated)
	const prURL = "https://github.com/acme/widgets/pull/9"
	if linked.id != 101 || linked.run.Title != "No docs/ folder" || linked.run.Conclusion != gate.ConclusionNeutral || !strings.Contains(linked.run.Summary, prURL) {
		t.Errorf("updated check run = %+v, want check run 101 neutral \"No docs/ folder\" linking %s", linked, prURL)
	}

	if code := postSigned(t, handler, secret, "d2", e2ePullRequestBody(t, 2, tip)); code != http.StatusAccepted {
		t.Fatalf("second POST /webhook = %d, want %d", code, http.StatusAccepted)
	}
	second := waitCall(t, gh.created)
	if !strings.Contains(second.run.Summary, prURL) || second.run.Conclusion != gate.ConclusionNeutral || second.run.Title != "No docs/ folder" {
		t.Errorf("second PR check run = %+v, want neutral \"No docs/ folder\" linking %s", second.run, prURL)
	}
	if err := stop(); err != nil {
		t.Fatalf("worker.Run() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if gh.creates != 1 {
		t.Errorf("CreateBranch calls = %d, want 1", gh.creates)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(gh.prs))
	}
	if want := (gate.NewPullRequest{Title: gh.prs[0].Title, Body: gh.prs[0].Body, Head: "pollux-agent/docs-scaffold", Base: "main"}); gh.prs[0] != want {
		t.Errorf("pull request = %+v, want head pollux-agent/docs-scaffold into main", gh.prs[0])
	}
	wantCommit := scaffoldCommit{
		branch: "pollux-agent/docs-scaffold",
		parent: tip,
		files:  map[string]string{"docs/README.md": scaffoldIndex, "docs/architecture.md": scaffoldArchitecture, "docs/guides/setup.md": scaffoldSetup},
	}
	if diff := cmp.Diff([]scaffoldCommit{wantCommit}, gh.commits, cmp.AllowUnexported(scaffoldCommit{})); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}

	if len(model.requests) != 3 {
		t.Fatalf("model saw %d requests, want 3 (list_dir, rejected submit_docs, accepted submit_docs)", len(model.requests))
	}
	if !slices.ContainsFunc(model.requests[0].Tools, func(tool llm.Tool) bool { return tool.Name == "submit_docs" }) {
		t.Errorf("first model request offers no submit_docs tool: %+v", model.requests[0].Tools)
	}
	listing := lastToolResult(model.requests[1])
	for _, want := range []string{"Makefile", "cmd/", "internal/"} {
		if !strings.Contains(listing.Content, want) {
			t.Errorf("list_dir result = %q, want it to contain %q", listing.Content, want)
		}
	}
	if rejected := lastToolResult(model.requests[2]); !rejected.IsError || !strings.Contains(rejected.Content, "link") {
		t.Errorf("rejected submit_docs result = %+v, want an error about the index links", rejected)
	}
}

func TestWebhookAdoptsTheBotsScaffoldPullRequestAfterStateLoss(t *testing.T) {
	adoptBotScaffoldPullRequest(t, "bot-commit")
}

func TestWebhookAdoptsAClosedBotScaffoldPullRequestWithoutItsBranch(t *testing.T) {
	adoptBotScaffoldPullRequest(t, "")
}

// adoptBotScaffoldPullRequest runs a fresh store against a repo whose scaffold
// branch is at branchTip (absent when empty) with a bot pull request from it.
func adoptBotScaffoldPullRequest(t *testing.T, branchTip string) {
	t.Helper()

	repoDir, tip := newGitRepo(t, map[string]string{
		"Makefile":        "test:\n\tgo test ./...\n",
		"cmd/app/main.go": "package main\n\nfunc main() {}\n",
	})
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")

	secret := []byte("test-secret")
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := newScaffoldGitHub(tip)
	if branchTip != "" {
		gh.branches["pollux-agent/docs-scaffold"] = branchTip
	}
	gh.existing = &gate.ScaffoldPR{Number: 7, URL: "https://github.com/acme/widgets/pull/7", ByBot: true}
	model := &scaffoldModel{}
	noToken := func(context.Context, int64, string) (string, error) { return "", nil }
	gateSvc := gate.NewService(gh, gh, store, gate.Runners{Server: newServerRunner(model, noToken)}, gh, httpapi.NewScaffoldQueue(store))
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), slog.New(slog.DiscardHandler), 8)
	stopWorker := runWorker(worker)
	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() { stopErr = stopWorker() })
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	handler := httpapi.NewHandler(httpapi.Deps{Logger: slog.New(slog.DiscardHandler), WebhookSecret: secret, Jobs: worker, Runs: store})

	if code := postSigned(t, handler, secret, "d1", e2ePullRequestBody(t, 1, tip)); code != http.StatusAccepted {
		t.Fatalf("POST /webhook = %d, want %d", code, http.StatusAccepted)
	}
	waitCall(t, gh.created)
	linked := waitCall(t, gh.updated)
	if !strings.Contains(linked.run.Summary, "https://github.com/acme/widgets/pull/7") {
		t.Errorf("updated check run = %+v, want it linking pull request 7", linked)
	}
	if err := stop(); err != nil {
		t.Fatalf("worker.Run() error = %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if branchTip == "" && gh.creates != 0 {
		t.Errorf("branch creations = %d, want none", gh.creates)
	}
	if gh.resets != 0 || len(gh.prs) != 0 || len(gh.commits) != 0 {
		t.Errorf("resets = %d, new pull requests = %d, commits = %d, want none", gh.resets, len(gh.prs), len(gh.commits))
	}
}

func lastToolResult(req llm.Request) llm.ToolResult {
	last := req.Messages[len(req.Messages)-1]
	if len(last.ToolResults) == 0 {
		return llm.ToolResult{}
	}
	return last.ToolResults[0]
}
