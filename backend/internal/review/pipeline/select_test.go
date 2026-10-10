package pipeline_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// A nested file matched by a ** covers glob reaches triage with its doc and
// patch; a doc whose covers do not match is not triaged.
func TestStart_GlobCoveredNestedFileTriagesOnlyItsDoc(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().
		doc("docs/a.md", "---\ntitle: A\nsummary: Describes A.\ncovers:\n  - src/**/*.go\n---\n# A\n\nold.\n").
		doc("docs/b.md", "---\ntitle: B\nsummary: Describes B.\ncovers:\n  - other/*.go\n---\n# B\n\nold.\n")
	const patch = "@@ -1,2 +1,3 @@\n package deep\n+func X() {}\n"
	changed := review.ChangedFile{Path: "src/pkg/deep/x.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: patch}

	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{}, changed)
	a.noImpact(t)
	prompts := a.judge.prompts(pipeline.KindTriage)
	if len(a.judge.asked) != 1 {
		t.Fatalf("judge saw %d questions, want exactly 1 triage call (docs/a.md only)", len(a.judge.asked))
	}
	for _, want := range []string{"docs/a.md", review.NumberedPatch(patch)} {
		if !strings.Contains(prompts[0], want) {
			t.Errorf("triage prompt missing %q:\n%s", want, prompts[0])
		}
	}
	if strings.Contains(prompts[0], "docs/b.md") {
		t.Errorf("triage prompt mentions uncovered docs/b.md:\n%s", prompts[0])
	}
}

func TestStart_DocThatDropsItsCoversInThePRIsStillTriagedFromHead(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		covers string
	}{
		{name: "emptied", covers: " []"},
		{name: "narrowed to a glob that misses the changed file", covers: "\n  - cmd/**"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ws := newWorkspace().
				baseDoc("docs/x.md", docWithCovers("\n  - main.go", "old behavior.")).
				headFile("docs/x.md", docWithCovers(tc.covers, "head-only body."))

			a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{})
			if len(a.judge.asked) != 1 {
				t.Fatalf("judge saw %d questions, want 1 triage call for docs/x.md", len(a.judge.asked))
			}
			prompt := a.judge.asked[0].prompt
			for _, want := range []string{"docs/x.md", "head-only body."} {
				if !strings.Contains(prompt, want) {
					t.Errorf("triage prompt = %q, want it to contain %q", prompt, want)
				}
			}
			if strings.Contains(prompt, "old behavior.") {
				t.Errorf("triage prompt = %q, want the head text of docs/x.md, not the base text", prompt)
			}
		})
	}
}

func TestStart_DocAddedByThePRIsNotACandidate(t *testing.T) {
	t.Parallel()

	t.Run("covered base doc is triaged and the added one is not", func(t *testing.T) {
		t.Parallel()

		ws := newWorkspace().
			baseDoc("docs/x.md", docWithCovers("\n  - main.go", "old behavior.")).
			headFile("docs/x.md", docWithCovers(" []", "x body.")).
			headFile("docs/new.md", docWithCovers("\n  - main.go", "new body."))

		a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{})
		if len(a.judge.asked) != 1 {
			t.Fatalf("judge saw %d questions, want 1 triage call for docs/x.md only", len(a.judge.asked))
		}
		prompt := a.judge.asked[0].prompt
		if !strings.Contains(prompt, "x body.") || strings.Contains(prompt, "new body.") {
			t.Errorf("triage prompt = %q, want docs/x.md and not docs/new.md", prompt)
		}
	})

	t.Run("uncovered base doc does not become a candidate", func(t *testing.T) {
		t.Parallel()

		ws := newWorkspace().
			doc("docs/other.md", docWithCovers("\n  - other.go", "other.")).
			headFile("docs/new.md", docWithCovers("\n  - main.go", "new."))

		a := mustAnalyze(t, ws, newJudge().on(pipeline.KindNewDoc, newDoc(false)), &fakeBackend{})
		a.noImpact(t)
		if len(a.judge.asked) != 1 || a.judge.asked[0].kind != pipeline.KindNewDoc {
			t.Fatalf("judge saw %+v, want only the new-doc decision (docs/new.md is not a candidate)", a.judge.asked)
		}
		if prompt := a.judge.asked[0].prompt; !strings.Contains(prompt, "main.go") {
			t.Errorf("new-doc prompt = %q, want it to list main.go as uncovered", prompt)
		}
	})
}

func TestStart_RenamedCoveringDocIsTriagedAtItsNewPath(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().
		baseDoc("docs/x.md", xDoc).
		headFile("docs/renamed.md", docWithCovers("\n  - main.go", "renamed body."))

	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{},
		mainGoChange(), review.ChangedFile{Path: "docs/renamed.md", PreviousPath: "docs/x.md"})
	if len(a.judge.asked) != 1 {
		t.Fatalf("judge saw %d questions, want 1 triage call for docs/renamed.md", len(a.judge.asked))
	}
	if prompt := a.judge.asked[0].prompt; !strings.Contains(prompt, "docs/renamed.md") {
		t.Errorf("triage prompt = %q, want it to name docs/renamed.md", prompt)
	}
}

func TestStart_RenameMatchesDocCoveringOnlyOldPath(t *testing.T) {
	t.Parallel()

	changed := mainGoChange()
	changed.Path, changed.PreviousPath = "renamed.go", "main.go"

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{}, changed)
	if len(a.judge.asked) != 1 || a.judge.asked[0].kind != pipeline.KindTriage {
		t.Fatalf("judge saw %+v, want 1 triage call for docs/x.md", a.judge.asked)
	}
}

func TestStart_DeletedCoveringDocIsRestoredWithoutModelCalls(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().
		baseDoc("docs/x.md", xDoc).
		baseDoc("docs/README.md", "## Index\n\n- [X](x.md): about x.\n")

	a := mustAnalyze(t, ws, newJudge(), &fakeBackend{}, mainGoChange(), review.ChangedFile{Path: "docs/x.md", Removed: true})
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
	p := a.proposals(t, 1)[0]
	if p.DocPath != "docs/x.md" || p.Section != "" || !strings.Contains(p.Content, "old behavior.") || p.IndexEntry != "- [X](x.md): about x." {
		t.Errorf("restore proposal = %+v, want docs/x.md recreated from base with its README entry", p)
	}
	if p.Anchor != (review.Anchor{File: "main.go", Line: 1}) {
		t.Errorf("Anchor = %v, want main.go:1", p.Anchor)
	}
}

func TestStart_DeletedDocWhoseCoveredFileIsAlsoDeletedIsNotRestored(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge(), &fakeBackend{},
		review.ChangedFile{Path: "main.go", Removed: true}, review.ChangedFile{Path: "docs/x.md", Removed: true})
	a.noImpact(t)
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
}

func TestStart_RemovalsAndDocsOnlyAreNoImpactWithoutModelCalls(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge(), &fakeBackend{},
		review.ChangedFile{Path: "gone.go", Removed: true},
		review.ChangedFile{Path: "docs/new.md", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -0,0 +1,3 @@\n+x\n"})
	if got := a.noImpact(t).Reason; got != basedocs.NothingToReview {
		t.Errorf("Reason = %q, want %q", got, basedocs.NothingToReview)
	}
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
}

func TestStart_NoChangedFilesIsNoImpactWithoutCheckoutOrModel(t *testing.T) {
	t.Parallel()

	backend := &fakeBackend{ws: xWorkspace()}
	judge := newJudge()
	started, err := newSync(backend, judge).Start(t.Context(), review.Request{Owner: "o", Repo: "r", Number: 1, HeadSHA: "deadbeef"})
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	if _, ok := result.Verdict.(review.NoImpact); !ok {
		t.Fatalf("Verdict = %T, want review.NoImpact", result.Verdict)
	}
	if len(backend.opened) != 0 || len(judge.asked) != 0 {
		t.Errorf("opened %d checkouts and asked %d questions, want none", len(backend.opened), len(judge.asked))
	}
}

func TestStart_TooManyCandidateDocsIsAnError(t *testing.T) {
	t.Parallel()

	ws := newWorkspace()
	for i := range basedocs.MaxCandidates + 1 {
		ws.doc(fmt.Sprintf("docs/d%d.md", i), xDoc)
	}
	a := analyze(t, ws, newJudge(), &fakeBackend{})
	if a.err == nil || !strings.Contains(a.err.Error(), fmt.Sprintf("cap of %d", basedocs.MaxCandidates)) {
		t.Fatalf("Start() = %v, want an error naming the candidate cap", a.err)
	}
	if got := a.failure(t).Cause; got != review.CauseTooManyCandidates {
		t.Errorf("Cause = %q, want %q", got, review.CauseTooManyCandidates)
	}
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
}

func TestStart_CandidateReplacedBySymlinkAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().baseDoc("docs/x.md", xDoc).at("docs/x.md", nonRegular())
	a := analyze(t, ws, newJudge(), &fakeBackend{})
	if got := a.failure(t).Cause; got != review.CauseInternal {
		t.Fatalf("Start() error = %v, want cause %q", a.err, review.CauseInternal)
	}
	if !strings.Contains(a.err.Error(), "docs/x.md") {
		t.Errorf("Start() error = %v, want it to name docs/x.md", a.err)
	}
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
}

func TestStart_CandidateUnderASymlinkedDirectoryAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().
		baseDoc("docs/sub/x.md", xDoc).
		headFile("docs/real/x.md", xDoc).
		at("docs/sub", nonRegular())
	a := analyze(t, ws, newJudge(), &fakeBackend{})
	if got := a.failure(t).Cause; got != review.CauseInternal {
		t.Fatalf("Start() error = %v, want cause %q", a.err, review.CauseInternal)
	}
	if a.calls() != 0 {
		t.Errorf("model saw %d calls, want 0", a.calls())
	}
}

func TestStart_DocWithBrokenFrontmatterAtHeadIsStillTriaged(t *testing.T) {
	t.Parallel()

	ws := xWorkspace().headFile("docs/x.md", "# X\n\n## Mid\nno frontmatter anymore.\n")
	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "Mid"

	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposal)}})
	if prompt := a.judge.asked[0].prompt; !strings.Contains(prompt, "no frontmatter anymore.") {
		t.Errorf("triage prompt = %q, want the raw head text of docs/x.md", prompt)
	}
	if want := "## Mid\nno frontmatter anymore.\n"; a.proposals(t, 1)[0].Original != want {
		t.Errorf("Original = %q, want %q", a.proposals(t, 1)[0].Original, want)
	}
}

func TestStart_EditedCandidateIsQuotedAtHeadNotBase(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().
		baseDoc("docs/x.md", docWithCovers("\n  - main.go", "## Mid\nbase mid text\n")).
		headFile("docs/x.md", docWithCovers(" []", "## Mid\nhead mid text\n"))
	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "Mid"

	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposal)}})
	if want := "## Mid\nhead mid text\n\n"; a.proposals(t, 1)[0].Original != want {
		t.Errorf("Original = %q, want head text %q", a.proposals(t, 1)[0].Original, want)
	}
}

func TestStart_DocsFileAtHeadIsAbsentReadme(t *testing.T) {
	t.Parallel()

	ws := newWorkspace().at("docs", file("not a directory\n"))
	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindNewDoc, newDoc(false)), &fakeBackend{}, otherGoChange())
	if len(a.judge.asked) != 1 || a.judge.asked[0].kind != pipeline.KindNewDoc {
		t.Fatalf("judge saw %+v, want exactly 1 new-doc decision", a.judge.asked)
	}
}
