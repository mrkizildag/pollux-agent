package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// headEntry is what the fake head holds at one path.
type headEntry struct {
	kind    finalize.Kind
	regular bool
	src     string
	err     error
}

func file(src string) headEntry { return headEntry{kind: finalize.Other, regular: true, src: src} }

// nonRegular is a symlink or a submodule: it exists but is never read.
func nonRegular() headEntry { return headEntry{kind: finalize.Other} }

// unreadable fails every Stat and ReadFile of the path with err.
func unreadable(err error) headEntry { return headEntry{err: err} }

// fakeWorkspace is an in-memory pipeline.Workspace: docs at the base as an
// fs.FS and the head as a path-to-entry map. A path that is not an entry but
// is the parent of one is a directory.
type fakeWorkspace struct {
	base    fstest.MapFS
	head    map[string]headEntry
	baseErr error
}

func newWorkspace() *fakeWorkspace {
	return &fakeWorkspace{base: fstest.MapFS{}, head: map[string]headEntry{}}
}

// doc puts the same doc at the base and the head.
func (w *fakeWorkspace) doc(path, src string) *fakeWorkspace {
	return w.baseDoc(path, src).headFile(path, src)
}

func (w *fakeWorkspace) baseDoc(path, src string) *fakeWorkspace {
	w.base[path] = &fstest.MapFile{Data: []byte(src)}
	return w
}

func (w *fakeWorkspace) headFile(path, src string) *fakeWorkspace {
	return w.at(path, file(src))
}

func (w *fakeWorkspace) at(path string, e headEntry) *fakeWorkspace {
	w.head[path] = e
	return w
}

func (w *fakeWorkspace) BaseDocs(context.Context) (fs.FS, error) {
	if w.baseErr != nil {
		return nil, w.baseErr
	}
	return w.base, nil
}

func (w *fakeWorkspace) Stat(_ context.Context, path string) (finalize.Kind, error) {
	if e, ok := w.head[path]; ok {
		return e.kind, e.err
	}
	for p := range w.head {
		if strings.HasPrefix(p, path+"/") {
			return finalize.Dir, nil
		}
	}
	return finalize.Missing, nil
}

func (w *fakeWorkspace) ReadFile(_ context.Context, path string) ([]byte, bool, error) {
	e, ok := w.head[path]
	switch {
	case !ok:
		return nil, false, nil
	case e.err != nil:
		return nil, false, e.err
	case !e.regular || len(e.src) > pipeline.MaxDocBytes:
		return nil, false, nil
	}
	return []byte(e.src), true, nil
}

// fakeBackend is a scripted pipeline.Backend: Run feeds submissions through
// Task.Accept in order until one is accepted.
type fakeBackend struct {
	ws      pipeline.Workspace
	openErr error
	model   string
	// submissions are the raw submit_proposals arguments, fed in order.
	submissions []json.RawMessage
	// charge is billed to the task's Meter when Run starts.
	charge review.Tokens
	// runErr is returned when no submission is accepted.
	runErr error

	opened   []pipeline.Checkout
	tasks    []pipeline.Task
	feedback []error
}

func (b *fakeBackend) Name() string { return "fake" }

func (b *fakeBackend) Open(_ context.Context, c pipeline.Checkout) (pipeline.Workspace, func(), error) {
	b.opened = append(b.opened, c)
	if b.openErr != nil {
		return nil, nil, b.openErr
	}
	return b.ws, func() {}, nil
}

func (b *fakeBackend) Run(ctx context.Context, _ pipeline.Workspace, t pipeline.Task) (pipeline.Output, error) {
	b.tasks = append(b.tasks, t)
	if err := t.Meter.Charge(b.charge); err != nil {
		return pipeline.Output{}, fmt.Errorf("fake run: %w", err)
	}
	for _, raw := range b.submissions {
		feedback, fatal := t.Accept(ctx, raw)
		b.feedback = append(b.feedback, feedback)
		if fatal != nil {
			return pipeline.Output{}, fmt.Errorf("fake run: %w", fatal)
		}
		if feedback == nil {
			return pipeline.Output{Raw: raw, Model: b.model}, nil
		}
	}
	if b.runErr != nil {
		return pipeline.Output{}, b.runErr
	}
	return pipeline.Output{}, errors.New("fake run: no submission was accepted")
}

// lastFeedback is the text the model got for its most recent rejected submission.
func (b *fakeBackend) lastFeedback(t *testing.T) string {
	t.Helper()
	for i := len(b.feedback) - 1; i >= 0; i-- {
		if b.feedback[i] != nil {
			return b.feedback[i].Error()
		}
	}
	t.Fatal("no submission was returned to the model")
	return ""
}

// submit is one submission of proposals, as the model would send it.
func submit(proposals ...any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{"proposals": proposals})
	if err != nil {
		panic(fmt.Sprintf("marshal proposals: %v", err))
	}
	return raw
}

// Judge kinds the pipeline asks.
const (
	kindTriage = "triage"
	kindNewDoc = "new_doc"
	kindVerify = "verify"
)

// reply is one scripted Judge answer.
type reply struct {
	text   string
	err    error
	tokens review.Tokens
}

func (r reply) withTokens(t review.Tokens) reply { r.tokens = t; return r }

func text(s string) reply { return reply{text: s} }

func triage(impacted bool) reply {
	return text(fmt.Sprintf(`{"impacted": %t, "reason": "scripted"}`, impacted))
}

func newDoc(needed bool) reply {
	return text(fmt.Sprintf(`{"needed": %t, "reason": "scripted"}`, needed))
}

func verify(supported bool) reply {
	return text(fmt.Sprintf(`{"supported": %t, "reason": "scripted"}`, supported))
}

// asked is one prompt the fake Judge saw.
type asked struct{ kind, system, prompt string }

// fakeJudge answers per kind from a script, in order, charging the Meter.
type fakeJudge struct {
	model   string
	replies map[string][]reply
	asked   []asked
}

func newJudge() *fakeJudge {
	return &fakeJudge{model: "triage-model", replies: map[string][]reply{}}
}

func (j *fakeJudge) on(kind string, rs ...reply) *fakeJudge {
	j.replies[kind] = append(j.replies[kind], rs...)
	return j
}

func (j *fakeJudge) Model() string { return j.model }

func (j *fakeJudge) Ask(_ context.Context, q pipeline.Question, m *pipeline.Meter) (string, error) {
	j.asked = append(j.asked, asked{kind: q.Kind, system: q.System, prompt: q.Prompt})
	rs := j.replies[q.Kind]
	if len(rs) == 0 {
		return "", fmt.Errorf("fakeJudge: unexpected %s question", q.Kind)
	}
	r := rs[0]
	j.replies[q.Kind] = rs[1:]
	if err := m.Charge(r.tokens); err != nil {
		return "", fmt.Errorf("fake charge: %w", err)
	}
	return r.text, r.err
}

// prompts are the prompts of the questions of one kind.
func (j *fakeJudge) prompts(kind string) []string {
	var out []string
	for _, a := range j.asked {
		if a.kind == kind {
			out = append(out, a.prompt)
		}
	}
	return out
}

// Fixture docs and changes.

const xDoc = "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n# X\n\nold behavior.\n"

func docWithCovers(covers, body string) string {
	return "---\ntitle: X\nsummary: Describes X.\ncovers:" + covers + "\n---\n# X\n\n" + body + "\n"
}

func mainGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"}
}

func otherGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "other.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -0,0 +1,3 @@\n+func other() {}\n"}
}

// xWorkspace is a repo whose only doc, docs/x.md, covers main.go.
func xWorkspace() *fakeWorkspace { return newWorkspace().doc("docs/x.md", xDoc) }

func proposalFor(docPath string, line int) map[string]any {
	return map[string]any{
		"doc_path": docPath,
		"section":  "X",
		"anchor":   map[string]any{"file": "main.go", "line": line},
		"reason":   "main.go's behavior changed",
		"content":  "new behavior.",
	}
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

func request(changed ...review.ChangedFile) review.Request {
	return review.Request{
		InstallationID: 1, Owner: "o", Repo: "r", Number: 1,
		HeadSHA: strings.Repeat("h", 40), BaseSHA: strings.Repeat("b", 40),
		ChangedFiles: changed,
	}
}

// analysis is one run of the pipeline over a fake backend and judge.
type analysis struct {
	backend *fakeBackend
	judge   *fakeJudge
	result  review.Result
	err     error
}

// analyze runs Sync over ws. With no changed files it analyzes main.go.
func analyze(t *testing.T, ws pipeline.Workspace, judge *fakeJudge, backend *fakeBackend, changed ...review.ChangedFile) analysis {
	t.Helper()
	return analyzeWith(t, pipeline.ReviewLimits(), ws, judge, backend, changed...)
}

func analyzeWith(t *testing.T, limits pipeline.Limits, ws pipeline.Workspace, judge *fakeJudge, backend *fakeBackend, changed ...review.ChangedFile) analysis {
	t.Helper()

	if len(changed) == 0 {
		changed = []review.ChangedFile{mainGoChange()}
	}
	backend.ws = ws
	if backend.model == "" {
		backend.model = "draft-model"
	}
	runner := newSync(backend, judge).WithLimits(limits)
	started, err := runner.Start(t.Context(), request(changed...))
	a := analysis{backend: backend, judge: judge, err: err}
	if err != nil {
		return a
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	a.result = result
	return a
}

// mustAnalyze fails the test when the run errors.
func mustAnalyze(t *testing.T, ws pipeline.Workspace, judge *fakeJudge, backend *fakeBackend, changed ...review.ChangedFile) analysis {
	t.Helper()
	a := analyze(t, ws, judge, backend, changed...)
	if a.err != nil {
		t.Fatalf("Start() = %v, want nil error", a.err)
	}
	return a
}

func (a analysis) noImpact(t *testing.T) review.NoImpact {
	t.Helper()
	v, ok := a.result.Verdict.(review.NoImpact)
	if !ok {
		t.Fatalf("Verdict = %#v, want review.NoImpact", a.result.Verdict)
	}
	return v
}

func (a analysis) proposals(t *testing.T, n int) review.Proposals {
	t.Helper()
	v, ok := a.result.Verdict.(review.Proposals)
	if !ok || len(v) != n {
		t.Fatalf("Verdict = %#v, want %d proposals", a.result.Verdict, n)
	}
	return v
}

// failure is the *review.FailedError the run returned.
func (a analysis) failure(t *testing.T) *review.FailedError {
	t.Helper()
	var failed *review.FailedError
	if !errors.As(a.err, &failed) {
		t.Fatalf("Start() = %v, want a *review.FailedError", a.err)
	}
	return failed
}

// calls is how many model calls the run made: judge questions plus draft tasks.
func (a analysis) calls() int { return len(a.judge.asked) + len(a.backend.tasks) }

func newSync(b pipeline.Backend, j pipeline.Judge) *pipeline.Sync {
	return pipeline.NewSync(b, j, slog.New(slog.DiscardHandler))
}

func cmpUsage(want, got *review.Usage) string { return cmp.Diff(want, got) }
