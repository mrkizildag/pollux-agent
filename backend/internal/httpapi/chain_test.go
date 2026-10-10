package httpapi_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

const chainPatch = "@@ -1,3 +1,3 @@\n package app\n-// old wording\n+// new wording\n"

type chainGitHub struct {
	noComments
	calls chan e2eCheckRunCall
}

func (f *chainGitHub) WorkflowExists(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func (f *chainGitHub) MergeBase(_ context.Context, _ int64, _, _, base, _ string) (string, error) {
	return base, nil
}

func (f *chainGitHub) DocsExist(context.Context, int64, string, string, string) (bool, error) {
	return true, nil
}

func (f *chainGitHub) ListChangedFiles(context.Context, int64, string, string, int) ([]review.ChangedFile, error) {
	return []review.ChangedFile{{
		Path:  "src/app.go",
		Hunks: []review.LineRange{{Start: 1, End: 3}},
		Patch: chainPatch,
	}}, nil
}

func (f *chainGitHub) CreateCheckRun(context.Context, int64, string, string, gate.CheckRun) (int64, error) {
	return 1, nil
}

func (f *chainGitHub) UpdateCheckRun(_ context.Context, installationID int64, owner, repo string, _ int64, run gate.CheckRun) error {
	f.calls <- e2eCheckRunCall{installationID: installationID, owner: owner, repo: repo, run: run}
	return nil
}

type chainModel struct {
	requests []llm.Request
}

func (m *chainModel) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	m.requests = append(m.requests, req)
	return llm.Response{Text: `{"impacted":false,"reason":"wording only"}`}, nil
}

// newChainRepo returns a repository whose base commit has a doc covering
// src/app.go and whose head commit rewords src/app.go.
func newChainRepo(t *testing.T) (repoDir, baseSHA, headSHA string) {
	t.Helper()

	repoDir, baseSHA = newGitRepo(t, map[string]string{
		"src/app.go":  "package app\n\n// old wording\n",
		"docs/app.md": "---\ntitle: App\nsummary: Describes the app.\ncovers:\n  - \"src/**\"\n---\n# App\n\n## Behavior\n\nThe app greets users.\n",
	})
	if err := os.WriteFile(filepath.Join(repoDir, "src/app.go"), []byte("package app\n\n// new wording\n"), 0o600); err != nil {
		t.Fatalf("write src/app.go: %v", err)
	}
	for _, args := range [][]string{{"commit", "-q", "-a", "-m", "reword"}, {"rev-parse", "HEAD"}} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		headSHA = strings.TrimSpace(string(out))
	}
	return repoDir, baseSHA, headSHA
}

// newGitRepo commits files to a new repository and returns its directory and head SHA.
func newGitRepo(t *testing.T, files map[string]string) (string, string) {
	t.Helper()

	dir := t.TempDir()
	git := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test-fixture git args are literals in this file
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return out
	}

	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	git("init", "-q", "-b", "main")
	// A detached auto-maintenance can outlive the test and break TempDir cleanup.
	git("config", "maintenance.auto", "false")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "test")
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	return dir, strings.TrimSpace(string(git("rev-parse", "HEAD")))
}

func TestWebhookToServerRunnerChain(t *testing.T) {
	repoDir, baseSHA, headSHA := newChainRepo(t)

	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")

	secret := []byte("test-secret")
	dbPath := filepath.Join(t.TempDir(), "pollux.db")
	store, err := sqlite.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open(%q) error = %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	gh := &chainGitHub{calls: make(chan e2eCheckRunCall, 1)}
	model := &chainModel{}
	runner := newServerRunner(model)
	gateSvc := gate.NewService(gh, unusedCommentGitHub{}, store, gate.Runners{Server: runner}, nil, nil)

	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(gateSvc), logger, 8)

	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(httpapi.Deps{Logger: logger, WebhookSecret: secret, Jobs: worker, Runs: store})
	body := e2ePullRequestFrom(t, 1, headSHA, pushOpts{baseSHA: baseSHA})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("X-GitHub-Delivery", "d1")
	req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /webhook = %d, want %d", rec.Code, http.StatusAccepted)
	}

	call := waitCheckRun(t, gh.calls)
	if call.run.Conclusion != gate.ConclusionSuccess || call.run.Title != "No doc impact" {
		t.Errorf("check run = %q %q, want %q %q", call.run.Conclusion, call.run.Title, gate.ConclusionSuccess, "No doc impact")
	}

	if len(model.requests) != 1 {
		t.Fatalf("model saw %d requests, want 1 triage request", len(model.requests))
	}
	var seen strings.Builder
	seen.WriteString(model.requests[0].System)
	for _, m := range model.requests[0].Messages {
		seen.WriteString(m.Text)
	}
	for _, want := range []string{"docs/app.md", "Describes the app.", review.NumberedPatch(chainPatch)} {
		if !strings.Contains(seen.String(), want) {
			t.Errorf("triage request does not contain %q:\n%s", want, seen.String())
		}
	}
}

// newServerRunner composes the server runner as cmd/server does.
func newServerRunner(model llm.Model) *pipeline.Sync {
	noToken := func(context.Context, int64, string) (string, error) { return "", nil }
	log := slog.New(slog.DiscardHandler)
	return pipeline.NewSync(llmrunner.NewBackend(model, noToken, "draft"), llmrunner.NewJudge(model, "triage"), log)
}
