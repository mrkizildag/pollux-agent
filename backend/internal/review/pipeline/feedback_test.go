package pipeline_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestStart_UncoveredFileWithoutNeedIsNoImpactAfterOneCall(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindNewDoc, newDoc(false)), &fakeBackend{}, otherGoChange())
	reason := a.noImpact(t).Reason
	for _, want := range []string{"other.go", "no new doc needed", "no doc covers"} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", reason, want)
		}
	}
	if len(a.judge.asked) != 1 || len(a.backend.tasks) != 0 {
		t.Fatalf("judge saw %d questions and backend ran %d tasks, want exactly 1 new-doc decision", len(a.judge.asked), len(a.backend.tasks))
	}
	if prompt := a.judge.asked[0].prompt; !strings.Contains(prompt, "other.go") || !strings.Contains(prompt, "func other() {}") {
		t.Errorf("new-doc prompt = %q, want the uncovered file and its patch", prompt)
	}
}

func TestStart_UncoveredFileThatNeedsADocGetsANewDocProposal(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindNewDoc, newDoc(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(newDocProposal("other.go"))}}, otherGoChange())
	p := a.proposals(t, 1)[0]
	if p.DocPath != "docs/other.md" || p.Section != "" || p.IndexEntry == "" || p.Original != "" {
		t.Errorf("proposal = %+v, want a new doc docs/other.md with an index entry", p)
	}
	for _, want := range []string{"---\n", "title:", "covers:", "other.go"} {
		if !strings.Contains(p.Content, want) {
			t.Errorf("new-doc Content = %q, want frontmatter containing %q", p.Content, want)
		}
	}
	if prompt := a.backend.tasks[0].Prompt; !strings.Contains(prompt, "no doc covers") || !strings.Contains(prompt, "other.go") {
		t.Errorf("draft prompt = %q, want it to list the uncovered file", prompt)
	}
}

func TestStart_MixedChangeTriagesCandidatesAndDecidesNewDoc(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindNewDoc, newDoc(true)).on(pipeline.KindVerify, verify(true), verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2), newDocProposal("other.go"))}},
		mainGoChange(), otherGoChange())
	a.proposals(t, 2)
	if a.calls() != 5 {
		t.Errorf("model saw %d calls, want 5 (triage, new-doc, draft, 2 verifies)", a.calls())
	}
}

func TestStart_MixedChangeWithUnimpactedCandidateStillGetsNewDoc(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(false)).on(pipeline.KindNewDoc, newDoc(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(newDocProposal("other.go"))}},
		mainGoChange(), otherGoChange())
	if p := a.proposals(t, 1)[0]; p.DocPath != "docs/other.md" || p.Section != "" {
		t.Errorf("proposal = %+v, want new doc docs/other.md", p)
	}
}

func TestStart_ChangedPathWithNewlineIsQuotedInAnchorHunks(t *testing.T) {
	t.Parallel()

	evil := otherGoChange()
	evil.Path = "evil\nInjected: line.go"
	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindNewDoc, newDoc(true)),
		&fakeBackend{submissions: []json.RawMessage{submit()}}, evil)
	prompt := a.backend.tasks[0].Prompt
	if want := `"evil\nInjected: line.go": 1-3`; !strings.Contains(prompt, want) {
		t.Errorf("draft prompt = %q, want it to contain the quoted path line %q", prompt, want)
	}
	if strings.Contains(prompt, "\nInjected: line.go: ") {
		t.Errorf("draft prompt = %q, want no line started by the raw path tail", prompt)
	}
}

// newDocReturned runs a new-doc submission the model gets back, then an empty one.
func newDocReturned(t *testing.T, ws *fakeWorkspace, proposal map[string]any) string {
	t.Helper()

	a := mustAnalyze(t, ws,
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindNewDoc, newDoc(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposal), submit()}}, mainGoChange(), otherGoChange())
	return a.backend.lastFeedback(t)
}

func TestStart_NewDocWhoseCoversMissTheUncoveredFilesIsReturnedToModel(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindNewDoc, newDoc(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(newDocProposal("elsewhere.go")), submit()}}, otherGoChange())
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "proposal 0: covers match none") {
		t.Errorf("feedback = %q, want a proposal 0 error about covers", got)
	}
}

func TestStart_NewDocIsRejectedWhenNoneWasNeeded(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindNewDoc, newDoc(false)),
		&fakeBackend{submissions: []json.RawMessage{submit(newDocProposal("other.go")), submit()}}, mainGoChange(), otherGoChange())
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "not allowed") {
		t.Errorf("feedback = %q, want an error rejecting the new doc", got)
	}
}

func TestStart_HashOnlySectionCannotBypassNewDocChecks(t *testing.T) {
	t.Parallel()

	proposal := proposalFor("docs/x.md", 2)
	proposal["section"] = "#"
	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposal), submit()}})
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "proposal 0: section: must name a heading") {
		t.Errorf("feedback = %q, want an error rejecting the empty heading", got)
	}
	a.noImpact(t)
}

func TestStart_NewDocAtAnExistingPathIsReturnedToModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ws   *fakeWorkspace
		path string
	}{
		{name: "a doc", ws: xWorkspace(), path: "docs/x.md"},
		{name: "a directory", ws: xWorkspace().headFile("docs/other.md/keep.txt", "keep\n"), path: "docs/other.md"},
		{name: "a file the path goes under", ws: xWorkspace(), path: "docs/x.md/new.md"},
		{name: "an oversized file", ws: xWorkspace().headFile("docs/other.md", strings.Repeat("x", 2<<20)), path: "docs/other.md"},
		{name: "a symlink", ws: xWorkspace().at("docs/other.md", nonRegular()), path: "docs/other.md"},
		{name: "a submodule", ws: xWorkspace().at("docs/sub", nonRegular()), path: "docs/sub/new.md"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proposal := newDocProposal("other.go")
			proposal["doc_path"] = tc.path
			if got := newDocReturned(t, tc.ws, proposal); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
				t.Errorf("feedback = %q, want proposal 0 reported as already existing", got)
			}
		})
	}
}

func TestStart_HeadReadErrorFailsTheRun(t *testing.T) {
	t.Parallel()

	boom := errors.New("lstat: name too long")
	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/unreadable.md"
	ws := xWorkspace().at("docs/unreadable.md", unreadable(boom))

	a := analyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindNewDoc, newDoc(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposal), submit()}}, mainGoChange(), otherGoChange())
	if !errors.Is(a.err, boom) {
		t.Fatalf("Start() = %v, want an error wrapping the head read failure", a.err)
	}
	if got := a.failure(t).Cause; got != review.CauseInternal {
		t.Errorf("Cause = %q, want %q", got, review.CauseInternal)
	}
	if len(a.backend.feedback) != 1 {
		t.Errorf("backend saw %d submissions, want 1 (no retry after the head read failed)", len(a.backend.feedback))
	}
}

func TestStart_InvalidProposalIsReturnedToModel(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/x.md", 2)
	bad["anchor"] = map[string]any{"file": "main.go", "line": 99}
	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(bad), submit(proposalFor("docs/x.md", 2))}})
	if got := a.backend.feedback[0]; got == nil || !strings.Contains(got.Error(), `proposal 0: anchor.line 99: not a numbered line in the diff of "main.go"; commentable lines: 1-3`) {
		t.Errorf("feedback = %v, want a validation error listing the commentable lines", got)
	}
	if proposals := a.proposals(t, 1); proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want the resubmitted valid proposal", a.result.Verdict)
	}
}

func TestStart_UnknownSectionIsReturnedToModelWithHeadings(t *testing.T) {
	t.Parallel()

	bad := proposalFor("docs/x.md", 2)
	bad["section"] = "Nope"
	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(bad), submit(proposalFor("docs/x.md", 2))}})
	if got := a.backend.feedback[0]; got == nil || !strings.Contains(got.Error(), `"X"`) {
		t.Errorf("feedback = %v, want an error listing heading \"X\"", got)
	}
	a.proposals(t, 1)
}

func TestStart_DuplicateHeadingIsReturnedToModelAsAmbiguous(t *testing.T) {
	t.Parallel()

	ws := xWorkspace().headFile("docs/x.md", "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n# Top\n\n## X\none\n\n## X\ntwo\n")
	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2)), submit()}})
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "ambiguous") {
		t.Errorf("feedback = %q, want proposal 0 reported as ambiguous", got)
	}
}

func TestStart_SectionEditOfDocMissingAtHeadIsReturnedToModel(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/y.md", 2)), submit()}})
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "no such doc at head") {
		t.Errorf("feedback = %q, want proposal 0 reported as a doc missing at head", got)
	}
}

func TestStart_EveryBadProposalIsReturnedToModelWithItsIndex(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{
			submit(proposalFor("docs/y.md", 2), proposalFor("docs/x.md", 2), proposalFor("docs/z.md", 2)), submit(),
		}})
	got := a.backend.lastFeedback(t)
	for _, want := range []string{"proposal 0:", "proposal 2:"} {
		if !strings.Contains(got, want) {
			t.Errorf("feedback = %q, want it to contain %q", got, want)
		}
	}
	if strings.Contains(got, "proposal 1:") {
		t.Errorf("feedback = %q, want no problem for the valid proposal 1", got)
	}
}

func TestStart_MalformedSubmissionIsReturnedToModel(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{json.RawMessage(`{"proposals": "none"}`), submit()}})
	if got := a.backend.lastFeedback(t); !strings.Contains(got, "decode submit_proposals arguments") {
		t.Errorf("feedback = %q, want a decode error", got)
	}
}
