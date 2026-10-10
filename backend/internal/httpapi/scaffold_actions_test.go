package httpapi_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	ghclient "github.com/mrkizildag/pollux-agent/backend/internal/github"
	"github.com/mrkizildag/pollux-agent/backend/internal/httpapi"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/actions"
)

const scaffoldTip = "tip1"

// scaffoldSavedStore reports each saved ScaffoldState, so a test knows the gate
// has recorded the awaited run before the workflow_run webhook arrives.
type scaffoldSavedStore struct {
	*sqlite.Store
	saved chan gate.ScaffoldState
}

func (s *scaffoldSavedStore) SaveScaffold(ctx context.Context, state gate.ScaffoldState) error {
	if err := s.Store.SaveScaffold(ctx, state); err != nil {
		return fmt.Errorf("save scaffold: %w", err)
	}
	s.saved <- state
	return nil
}

// scaffoldAPI serves the GitHub API surface of a repo with the pollux-agent
// workflow, a default branch at scaffoldTip and no docs/, and records the
// dispatches, branches, commits and pull requests made against it.
type scaffoldAPI struct {
	t          *testing.T
	dispatched chan map[string]any
	created    chan map[string]any
	updated    chan map[string]any

	mu       sync.Mutex
	runs     int64
	checks   int64
	inputs   map[string]any
	blobs    map[string]string
	branches map[string]string
	creates  int
	commits  []map[string]string
	prs      []map[string]any
	blobURL  string
}

func (f *scaffoldAPI) json(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := fmt.Fprint(w, body); err != nil { //nolint:gosec // a test fake writing fixture JSON, not request input
		f.t.Errorf("write response: %v", err)
	}
}

func (f *scaffoldAPI) decode(r *http.Request) map[string]any {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("decode %s %s body: %v", r.Method, r.URL.Path, err)
	}
	return body
}

func (f *scaffoldAPI) resultZip() []byte {
	f.mu.Lock()
	nonce := f.inputs["nonce"]
	f.mu.Unlock()

	result, err := json.Marshal(map[string]any{
		"head_sha": scaffoldTip,
		"nonce":    nonce,
		"claude": map[string]any{
			"is_error":          false,
			"structured_output": map[string]string{"index": scaffoldIndex, "architecture": scaffoldArchitecture, "setup": scaffoldSetup},
		},
	})
	if err != nil {
		f.t.Errorf("marshal result: %v", err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	file, err := zw.Create("result.json")
	if err != nil {
		f.t.Errorf("create zip entry: %v", err)
	}
	if _, err := file.Write(result); err != nil {
		f.t.Errorf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		f.t.Errorf("close zip: %v", err)
	}
	return buf.Bytes()
}

func (f *scaffoldAPI) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"token":"ghs_test","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339)))
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/.github/workflows/pollux-agent.yml", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"type":"file","name":"pollux-agent.yml","path":".github/workflows/pollux-agent.yml"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/contents/docs", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusNotFound, `{"message":"Not Found"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"default_branch":"main"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/branches/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("branch")
		f.mu.Lock()
		sha, ok := f.branches[name]
		f.mu.Unlock()
		if !ok {
			f.json(w, http.StatusNotFound, `{"message":"Branch not found"}`)
			return
		}
		f.json(w, http.StatusOK, fmt.Sprintf(`{"name":%q,"commit":{"sha":%q}}`, name, sha))
	})
	mux.HandleFunc("POST /repos/acme/widgets/actions/workflows/pollux-agent.yml/dispatches", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.runs++
		id := 4241 + f.runs
		f.inputs, _ = body["inputs"].(map[string]any)
		f.mu.Unlock()
		f.dispatched <- body
		f.json(w, http.StatusOK, fmt.Sprintf(`{"workflow_run_id":%d}`, id))
	})
	mux.HandleFunc("POST /repos/acme/widgets/check-runs", func(w http.ResponseWriter, r *http.Request) {
		f.created <- f.decode(r)
		f.mu.Lock()
		f.checks++
		id := 100 + f.checks
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"id":%d}`, id))
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/check-runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		body["id"] = r.PathValue("id")
		f.updated <- body
		f.json(w, http.StatusOK, `{"id":1}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/runs/{run}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"total_count":1,"artifacts":[{"id":9,"name":"pollux-agent-result","workflow_run":{"id":%s}}]}`, r.PathValue("run")))
	})
	mux.HandleFunc("GET /repos/acme/widgets/actions/artifacts/9/zip", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, f.blobURL, http.StatusFound)
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write(f.resultZip()); err != nil {
			f.t.Errorf("write blob: %v", err)
		}
	})

	mux.HandleFunc("POST /repos/acme/widgets/git/refs", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		name := strings.TrimPrefix(fmt.Sprint(body["ref"]), "refs/heads/")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates++
		if _, ok := f.branches[name]; ok {
			f.json(w, http.StatusUnprocessableEntity, `{"message":"Reference already exists"}`)
			return
		}
		f.branches[name] = fmt.Sprint(body["sha"])
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"ref":%q,"object":{"sha":%q}}`, body["ref"], body["sha"]))
	})
	mux.HandleFunc("PATCH /repos/acme/widgets/git/refs/heads/{branch...}", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.branches[r.PathValue("branch")] = fmt.Sprint(body["sha"])
		f.mu.Unlock()
		f.json(w, http.StatusOK, `{"ref":"ok"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/commits/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"sha":%q,"tree":{"sha":"tree-of-%s"}}`, r.PathValue("sha"), r.PathValue("sha")))
	})
	mux.HandleFunc("GET /repos/acme/widgets/git/trees/{sha}", func(w http.ResponseWriter, r *http.Request) {
		f.json(w, http.StatusOK, fmt.Sprintf(`{"sha":%q,"tree":[{"path":"README.md","type":"blob","mode":"100644","sha":"readme"}]}`, r.PathValue("sha")))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/blobs", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		sha := fmt.Sprintf("blob-%d", len(f.blobs)+1)
		f.blobs[sha] = fmt.Sprint(body["content"])
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":%q}`, sha))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/trees", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		files := map[string]string{}
		entries, _ := body["tree"].([]any)
		f.mu.Lock()
		for _, e := range entries {
			entry, _ := e.(map[string]any)
			files[fmt.Sprint(entry["path"])] = f.blobs[fmt.Sprint(entry["sha"])]
		}
		f.commits = append(f.commits, files)
		n := len(f.commits)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":"tree-new-%d"}`, n))
	})
	mux.HandleFunc("POST /repos/acme/widgets/git/commits", func(w http.ResponseWriter, r *http.Request) {
		f.decode(r)
		f.mu.Lock()
		n := len(f.commits)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, fmt.Sprintf(`{"sha":"commit-%d"}`, n))
	})
	mux.HandleFunc("GET /app", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `{"id":1,"slug":"pollux-agent"}`)
	})
	mux.HandleFunc("GET /repos/acme/widgets/pulls", func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, http.StatusOK, `[]`)
	})
	mux.HandleFunc("POST /repos/acme/widgets/pulls", func(w http.ResponseWriter, r *http.Request) {
		body := f.decode(r)
		f.mu.Lock()
		f.prs = append(f.prs, body)
		f.mu.Unlock()
		f.json(w, http.StatusCreated, `{"number":9,"html_url":"https://github.com/acme/widgets/pull/9"}`)
	})
	return mux
}

type actionsScaffoldHarness struct {
	api   *scaffoldAPI
	saved chan gate.ScaffoldState
	post  func(event, deliveryID string, body []byte)
}

// newActionsScaffoldHarness serves a repo with the workflow; server, when not
// nil, is configured next to the Actions runner.
func newActionsScaffoldHarness(t *testing.T, server gate.ServerRunner) *actionsScaffoldHarness {
	t.Helper()

	secret := []byte("test-secret")
	baseStore, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "pollux.db"))
	if err != nil {
		t.Fatalf("sqlite.Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := baseStore.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})
	store := &scaffoldSavedStore{Store: baseStore, saved: make(chan gate.ScaffoldState, 20)}

	api := &scaffoldAPI{
		t: t, dispatched: make(chan map[string]any, 4), created: make(chan map[string]any, 8), updated: make(chan map[string]any, 8),
		blobs: map[string]string{}, branches: map[string]string{"main": scaffoldTip},
	}
	srv := httptest.NewServer(api.handler())
	t.Cleanup(srv.Close)
	api.blobURL = srv.URL + "/blob"

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	client, err := ghclient.NewClient(&http.Client{Timeout: 5 * time.Second}, 1, keyPEM, srv.URL)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	gateSvc := gate.NewService(client, client, store, gate.Runners{Actions: actions.New(client, 10*time.Minute, 10*time.Minute), Server: server}, client, httpapi.NewScaffoldQueue(baseStore))
	logger := slog.New(slog.DiscardHandler)
	worker := jobqueue.NewWorker(baseStore, httpapi.HandleJob(gateSvc), logger, 8)
	workerCtx, cancelWorker := context.WithCancel(t.Context())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		if err := <-workerDone; err != nil {
			t.Errorf("worker.Run() error = %v", err)
		}
	})

	handler := httpapi.NewHandler(httpapi.Deps{Logger: logger, WebhookSecret: secret, Jobs: worker, Runs: baseStore})
	return &actionsScaffoldHarness{api: api, saved: store.saved, post: func(event, deliveryID string, body []byte) {
		t.Helper()

		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/webhook", bytes.NewReader(body))
		req.Header.Set("X-GitHub-Event", event)
		req.Header.Set("X-GitHub-Delivery", deliveryID)
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("POST /webhook %s = %d, want %d", event, rec.Code, http.StatusAccepted)
		}
	}}
}

func waitMap(t *testing.T, ch chan map[string]any, what string) map[string]any {
	t.Helper()

	select {
	case body := <-ch:
		return body
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

// waitScaffoldState returns the first saved state satisfying ok.
func waitScaffoldState(t *testing.T, ch chan gate.ScaffoldState, what string, ok func(gate.ScaffoldState) bool) gate.ScaffoldState {
	t.Helper()

	for {
		select {
		case state := <-ch:
			if ok(state) {
				return state
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func checkRunOutput(body map[string]any) (title, summary string) {
	output, _ := body["output"].(map[string]any)
	title, _ = output["title"].(string)
	summary, _ = output["summary"].(string)
	return title, summary
}

// unusedModel fails the test if the server runner calls it.
type unusedModel struct{ t *testing.T }

func (m unusedModel) Complete(context.Context, llm.Request) (llm.Response, error) {
	m.t.Errorf("server model was called, want the repo's workflow to write the scaffold")
	return llm.Response{}, errors.New("unexpected server model call")
}

func TestWebhookToActionsScaffoldPullRequest(t *testing.T) {
	t.Parallel()
	testWebhookToActionsScaffoldPullRequest(t, nil)
}

func TestWebhookToActionsScaffoldPreferredOverServerRunner(t *testing.T) {
	t.Parallel()
	noToken := func(context.Context, int64, string) (string, error) { return "", nil }
	testWebhookToActionsScaffoldPullRequest(t, newServerRunner(unusedModel{t: t}, noToken))
}

func testWebhookToActionsScaffoldPullRequest(t *testing.T, server gate.ServerRunner) {
	t.Helper()

	h := newActionsScaffoldHarness(t, server)

	h.post("pull_request", "d1", e2ePullRequestBody(t, 1, "pr1sha"))
	created := waitMap(t, h.api.created, "the waiting check run")
	if title, summary := checkRunOutput(created); created["conclusion"] != "neutral" || title != "No docs/ folder" || !strings.Contains(summary, "writing") {
		t.Errorf("created check run = %v, want neutral \"No docs/ folder\" saying the folder is being written", created)
	}

	dispatched := waitMap(t, h.api.dispatched, "the workflow dispatch")
	inputs, _ := dispatched["inputs"].(map[string]any)
	if inputs["pr_number"] != "0" || inputs["head_sha"] != scaffoldTip || inputs["nonce"] == "" {
		t.Errorf("dispatch inputs = %v, want pr_number 0, head_sha %s and a nonce", inputs, scaffoldTip)
	}
	awaiting := waitScaffoldState(t, h.saved, "the awaited run", func(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldAwaiting })
	if awaiting.Run == nil || awaiting.Run.RunID != 4242 || awaiting.BaseSHA != scaffoldTip {
		t.Fatalf("awaiting state = %+v, want run 4242 started from %s", awaiting, scaffoldTip)
	}

	h.post("workflow_run", "d2", e2eWorkflowRunCompletedBody(t, 4242, "success"))
	linked := waitMap(t, h.api.updated, "the link in the waiting check run")
	title, summary := checkRunOutput(linked)
	const prURL = "https://github.com/acme/widgets/pull/9"
	if linked["id"] != "101" || linked["conclusion"] != "neutral" || title != "No docs/ folder" || !strings.Contains(summary, prURL) {
		t.Errorf("updated check run = %v, want check run 101 neutral \"No docs/ folder\" linking %s", linked, prURL)
	}

	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if h.api.creates != 1 {
		t.Errorf("branch creations = %d, want 1", h.api.creates)
	}
	wantFiles := map[string]string{"docs/README.md": scaffoldIndex, "docs/architecture.md": scaffoldArchitecture, "docs/guides/setup.md": scaffoldSetup}
	if diff := cmp.Diff([]map[string]string{wantFiles}, h.api.commits); diff != "" {
		t.Errorf("commits (-want +got):\n%s", diff)
	}
	if len(h.api.prs) != 1 {
		t.Fatalf("pull requests created = %d, want 1", len(h.api.prs))
	}
	if pr := h.api.prs[0]; pr["head"] != "pollux-agent/docs-scaffold" || pr["base"] != "main" {
		t.Errorf("pull request = %v, want pollux-agent/docs-scaffold into main", pr)
	}
}

func TestWebhookToActionsScaffoldRunFailure(t *testing.T) {
	t.Parallel()

	h := newActionsScaffoldHarness(t, nil)

	h.post("pull_request", "d1", e2ePullRequestBody(t, 1, "pr1sha"))
	waitMap(t, h.api.dispatched, "the first dispatch")
	waitScaffoldState(t, h.saved, "the awaited run", func(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldAwaiting })

	h.post("workflow_run", "d2", e2eWorkflowRunCompletedBody(t, 4242, "failure"))
	failed := waitScaffoldState(t, h.saved, "the failed attempt", func(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldIdle })
	if failed.Attempt != 1 || failed.Run != nil {
		t.Errorf("state after the failed run = %+v, want Idle, attempt 1, no run", failed)
	}

	h.post("pull_request", "d3", e2ePullRequestBody(t, 2, "pr2sha"))
	dispatched := waitMap(t, h.api.dispatched, "the second dispatch")
	if inputs, _ := dispatched["inputs"].(map[string]any); inputs["pr_number"] != "0" {
		t.Errorf("second dispatch inputs = %v, want pr_number 0", inputs)
	}
	retrying := waitScaffoldState(t, h.saved, "the second awaited run", func(s gate.ScaffoldState) bool { return s.Phase == gate.ScaffoldAwaiting })
	if retrying.Run == nil || retrying.Run.RunID != 4243 {
		t.Errorf("state after the second dispatch = %+v, want run 4243", retrying)
	}

	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	if len(h.api.prs) != 0 || h.api.creates != 0 || len(h.api.commits) != 0 {
		t.Errorf("after a failed run: %d pull requests, %d branch creations, %d commits, want none", len(h.api.prs), h.api.creates, len(h.api.commits))
	}
}
