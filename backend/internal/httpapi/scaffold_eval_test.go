package httpapi_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

// flakyScaffoldGitHub fails the first UpdateCheckRun and/or the first
// CreatePullRequest, then behaves like scaffoldGitHub.
type flakyScaffoldGitHub struct {
	*scaffoldGitHub
	failMu       sync.Mutex
	failUpdate   bool
	failCreatePR bool
}

func (f *flakyScaffoldGitHub) UpdateCheckRun(ctx context.Context, inst int64, owner, repo string, id int64, run gate.CheckRun) error {
	f.failMu.Lock()
	fail := f.failUpdate
	f.failUpdate = false
	f.failMu.Unlock()
	if fail {
		return errors.New("github: 502")
	}
	return f.scaffoldGitHub.UpdateCheckRun(ctx, inst, owner, repo, id, run)
}

func (f *flakyScaffoldGitHub) CreatePullRequest(ctx context.Context, inst int64, owner, repo string, pr gate.NewPullRequest) (gate.ScaffoldPR, error) {
	f.failMu.Lock()
	fail := f.failCreatePR
	f.failCreatePR = false
	f.failMu.Unlock()
	if fail {
		return gate.ScaffoldPR{}, errors.New("github: 502")
	}
	return f.scaffoldGitHub.CreatePullRequest(ctx, inst, owner, repo, pr)
}

// erroringFirstModel fails its first call, then submits valid docs on every call.
type erroringFirstModel struct {
	mu    sync.Mutex
	calls int
}

func (m *erroringFirstModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.calls == 1 {
		return llm.Response{}, errors.New("provider: 529 overloaded")
	}
	return llm.Response{ToolCalls: []llm.ToolCall{submitDocs("1", scaffoldIndex, scaffoldArchitecture, scaffoldSetup)}}, nil
}

type evalEnv struct {
	handler http.Handler
	secret  []byte
	stop    func() error
}

func startEvalEnv(t *testing.T, store *sqlite.Store, gh gateScaffoldGitHub, model llm.Model) evalEnv {
	t.Helper()
	svc := gate.NewService(gh, gh, store, gate.Runners{Server: newServerRunner(model)}, gh, httpapi.NewScaffoldQueue(store))
	worker := jobqueue.NewWorker(store, httpapi.HandleJob(svc), slog.New(slog.DiscardHandler), 8)
	stop := runWorker(worker)
	var once sync.Once
	var stopErr error
	stopFn := func() error { once.Do(func() { stopErr = stop() }); return stopErr }
	t.Cleanup(func() { _ = stopFn() })
	secret := []byte("test-secret")
	return evalEnv{handler: httpapi.NewHandler(httpapi.Deps{Logger: slog.New(slog.DiscardHandler), WebhookSecret: secret, Jobs: worker, Runs: store}), secret: secret, stop: stopFn}
}

type gateScaffoldGitHub interface {
	gate.GitHub
	gate.CommentGitHub
	gate.ScaffoldGitHub
}

func openEvalStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func evalRepo(t *testing.T) string {
	t.Helper()
	repoDir, tip := newGitRepo(t, map[string]string{"Makefile": "test:\n\tgo test ./...\n", "cmd/app/main.go": "package main\n\nfunc main() {}\n"})
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+repoDir+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://github.com/acme/widgets.git")
	return tip
}

func evalWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Criterion 4: two PRs at once, a redelivered webhook, and a restart open one scaffold PR.
func TestEvalScaffoldOnceUnderConcurrencyRedeliveryAndRestart(t *testing.T) {
	tip := evalRepo(t)
	store := openEvalStore(t)
	gh := newScaffoldGitHub(tip)
	gh.created = make(chan scaffoldCheckRun, 32)
	gh.updated = make(chan scaffoldCheckRun, 32)
	model := &erroringFirstModel{calls: 1}
	env := startEvalEnv(t, store, gh, model)

	var wg sync.WaitGroup
	for _, d := range []struct {
		id string
		n  int
	}{{"d1", 1}, {"d2", 2}, {"d1", 1}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			postSigned(t, env.handler, env.secret, d.id, e2ePullRequestBody(t, d.n, tip))
		}()
	}
	wg.Wait()
	evalWaitFor(t, "a scaffold PR", func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.prs) > 0 })
	if err := env.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	env2 := startEvalEnv(t, store, gh, model)
	postSigned(t, env2.handler, env2.secret, "d3", e2ePullRequestBody(t, 3, tip))
	postSigned(t, env2.handler, env2.secret, "d1", e2ePullRequestBody(t, 1, tip))
	evalWaitFor(t, "PR 3's check run", func() bool { return len(gh.created) >= 3 })
	if err := env2.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}

	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.prs) != 1 || len(gh.commits) != 1 {
		t.Errorf("pull requests = %d, commits = %d, want 1 and 1", len(gh.prs), len(gh.commits))
	}
	close(gh.created)
	close(gh.updated)
	linked := map[int64]bool{}
	for u := range gh.updated {
		if strings.Contains(u.run.Summary, "/pull/9") {
			linked[u.id] = true
		}
	}
	n := 0
	for c := range gh.created {
		n++
		if !strings.Contains(c.run.Summary, "/pull/9") && !linked[c.id] {
			t.Errorf("check run %d never links the scaffold PR: %q", c.id, c.run.Summary)
		}
	}
	if n != 3 {
		t.Errorf("check runs created = %d, want 3 (PRs 1, 2, 3; redelivered d1 deduplicated)", n)
	}
}

// Criterion 7: a server LLM error opens no PR and the next PR event retries.
func TestEvalScaffoldLLMErrorRetriesOnNextEvent(t *testing.T) {
	tip := evalRepo(t)
	store := openEvalStore(t)
	gh := newScaffoldGitHub(tip)
	env := startEvalEnv(t, store, gh, &erroringFirstModel{})

	postSigned(t, env.handler, env.secret, "d1", e2ePullRequestBody(t, 1, tip))
	first := waitCall(t, gh.created)
	if first.run.Title != "No docs/ folder" || first.run.Conclusion != gate.ConclusionNeutral {
		t.Errorf("first check run = %+v", first.run)
	}
	evalWaitFor(t, "the failed attempt", func() bool {
		s, err := store.LoadScaffold(t.Context(), "acme", "widgets")
		return err == nil && s.Attempt == 1 && s.Phase == gate.ScaffoldIdle
	})
	gh.mu.Lock()
	if len(gh.prs) != 0 {
		t.Errorf("pull requests after LLM error = %d, want 0", len(gh.prs))
	}
	gh.mu.Unlock()

	postSigned(t, env.handler, env.secret, "d2", e2ePullRequestBody(t, 2, tip))
	waitCall(t, gh.created)
	evalWaitFor(t, "the retried scaffold PR", func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.prs) == 1 })
}

// Criterion 7: a GitHub error creating the PR opens none; the next event opens exactly one.
func TestEvalScaffoldCreatePRErrorRetriesOnNextEvent(t *testing.T) {
	tip := evalRepo(t)
	store := openEvalStore(t)
	gh := &flakyScaffoldGitHub{scaffoldGitHub: newScaffoldGitHub(tip), failCreatePR: true}
	env := startEvalEnv(t, store, gh, &erroringFirstModel{calls: 1})

	postSigned(t, env.handler, env.secret, "d1", e2ePullRequestBody(t, 1, tip))
	waitCall(t, gh.created)
	evalWaitFor(t, "the failed attempt", func() bool {
		s, err := store.LoadScaffold(t.Context(), "acme", "widgets")
		return err == nil && s.Attempt == 1
	})
	postSigned(t, env.handler, env.secret, "d2", e2ePullRequestBody(t, 2, tip))
	waitCall(t, gh.created)
	evalWaitFor(t, "the scaffold PR", func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.prs) == 1 })
	if err := env.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.commits) != 1 {
		t.Errorf("commits = %d, want 1", len(gh.commits))
	}
}

// Criterion 5 edge: a transient failure linking a waiting check run must not
// leave that check without the scaffold PR link forever.
func TestEvalScaffoldLinkFailureIsHealedByNextEvent(t *testing.T) {
	tip := evalRepo(t)
	store := openEvalStore(t)
	gh := &flakyScaffoldGitHub{scaffoldGitHub: newScaffoldGitHub(tip), failUpdate: true}
	env := startEvalEnv(t, store, gh, &erroringFirstModel{calls: 1})

	postSigned(t, env.handler, env.secret, "d1", e2ePullRequestBody(t, 1, tip))
	first := waitCall(t, gh.created)
	evalWaitFor(t, "the scaffold PR", func() bool { gh.mu.Lock(); defer gh.mu.Unlock(); return len(gh.prs) == 1 })

	postSigned(t, env.handler, env.secret, "d2", e2ePullRequestBody(t, 2, tip))
	waitCall(t, gh.created)
	select {
	case u := <-gh.updated:
		if u.id != first.id || !strings.Contains(u.run.Summary, "/pull/9") {
			t.Errorf("update = %+v, want check run %d linked", u, first.id)
		}
	case <-time.After(5 * time.Second):
		t.Errorf("check run %d of PR 1 was never linked to the scaffold PR after its first link update failed", first.id)
	}
}

// alwaysErroringModel fails every call.
type alwaysErroringModel struct {
	mu    sync.Mutex
	calls int
}

func (m *alwaysErroringModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	return llm.Response{}, errors.New("provider: 529 overloaded")
}

func (m *alwaysErroringModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// Criterion 8 (modified): three failed attempts, each retried by the next PR
// event; after the third, the next PR's check says the scaffold could not be
// written and no further attempt runs the model.
func TestEvalScaffoldGivesUpAfterThreeFailedAttempts(t *testing.T) {
	tip := evalRepo(t)
	store := openEvalStore(t)
	gh := newScaffoldGitHub(tip)
	model := &alwaysErroringModel{}
	env := startEvalEnv(t, store, gh, model)
	drain := func() []scaffoldCheckRun {
		var out []scaffoldCheckRun
		for {
			select {
			case u := <-gh.updated:
				out = append(out, u)
			default:
				return out
			}
		}
	}

	for i := 1; i <= 3; i++ {
		postSigned(t, env.handler, env.secret, "d"+strconv.Itoa(i), e2ePullRequestBody(t, i, tip))
		c := waitCall(t, gh.created)
		if c.run.Title != "No docs/ folder" || c.run.Conclusion != gate.ConclusionNeutral {
			t.Errorf("PR %d check run = %+v, want neutral \"No docs/ folder\"", i, c.run)
		}
		evalWaitFor(t, "failed attempt", func() bool {
			s, err := store.LoadScaffold(t.Context(), "acme", "widgets")
			return err == nil && s.Failures == i
		})
		evalWaitFor(t, "waiters told", func() bool { return len(gh.updated) > 0 })
		time.Sleep(50 * time.Millisecond)
		ups := drain()
		want := "tries again"
		if i == 3 {
			want = "after 3 attempts"
		}
		for _, u := range ups {
			if u.run.Title != "No docs/ folder" || !strings.Contains(u.run.Summary, want) {
				t.Errorf("after failure %d, waiter update = %+v, want title \"No docs/ folder\" mentioning %q", i, u.run, want)
			}
		}
	}
	s, err := store.LoadScaffold(t.Context(), "acme", "widgets")
	if err != nil || s.Phase != gate.ScaffoldGaveUp {
		t.Fatalf("state after 3 failures = %+v, %v, want gave_up", s, err)
	}
	callsAtGiveUp := model.count()

	postSigned(t, env.handler, env.secret, "d4", e2ePullRequestBody(t, 4, tip))
	c := waitCall(t, gh.created)
	if c.run.Title != "No docs/ folder" || !strings.Contains(c.run.Summary, "could not write") {
		t.Errorf("PR 4 check run = %+v, want it to say the scaffold could not be written", c.run)
	}
	time.Sleep(500 * time.Millisecond)
	if err := env.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if model.count() != callsAtGiveUp {
		t.Errorf("model calls = %d after giving up, want %d (no further attempt)", model.count(), callsAtGiveUp)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if len(gh.prs) != 0 || len(gh.commits) != 0 {
		t.Errorf("pull requests = %d, commits = %d, want 0 and 0", len(gh.prs), len(gh.commits))
	}
}
