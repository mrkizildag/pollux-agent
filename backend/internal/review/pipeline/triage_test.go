package pipeline_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestStart_ImpactedDocProducesProposal(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2))}})
	p := a.proposals(t, 1)[0]
	if p.DocPath != "docs/x.md" || p.Anchor.File != "main.go" || p.Anchor.Line != 2 {
		t.Errorf("proposals[0] = %+v, want doc_path docs/x.md anchored at main.go:2", p)
	}
	if len(a.backend.tasks) != 1 {
		t.Fatalf("backend ran %d tasks, want 1 draft", len(a.backend.tasks))
	}
	if task := a.backend.tasks[0]; task.Finish.Name != "submit_proposals" || task.Accept == nil || task.Meter == nil {
		t.Errorf("draft task = %+v, want the submit_proposals finish, an Accept and a Meter", task)
	}
}

func TestStart_AllTriageNoIsNoImpact(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{})
	a.noImpact(t)
	if len(a.judge.asked) != 1 || len(a.backend.tasks) != 0 {
		t.Errorf("judge saw %d questions and backend ran %d tasks, want exactly 1 triage call and no draft", len(a.judge.asked), len(a.backend.tasks))
	}
}

func TestStart_CoveredFileIsTriagedWithItsPatch(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(false)), &fakeBackend{})
	if len(a.judge.asked) != 1 {
		t.Fatalf("judge saw %d questions, want 1 triage call", len(a.judge.asked))
	}
	for _, want := range []string{"docs/x.md", "@@ -1,2 +1,3 @@\n     1  func main() {}\n"} {
		if !strings.Contains(a.judge.asked[0].prompt, want) {
			t.Errorf("triage prompt = %q, want it to contain %q", a.judge.asked[0].prompt, want)
		}
	}
}

func TestStart_NoImpactReasonJoinsTriageReasons(t *testing.T) {
	t.Parallel()

	ws := xWorkspace().doc("docs/y.md", xDoc)
	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(false), triage(false)), &fakeBackend{})
	reason := a.noImpact(t).Reason
	for _, want := range []string{"no candidate doc is affected", "docs/x.md: scripted", "docs/y.md: scripted"} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", reason, want)
		}
	}
	if a.result.Model != "triage-model" {
		t.Errorf("Model = %q, want triage-model", a.result.Model)
	}
}

func TestStart_FencedTriageReplyParses(t *testing.T) {
	t.Parallel()

	judge := newJudge().on(pipeline.KindTriage, text("Sure:\n```json\n{\"impacted\": false, \"reason\": \"fenced\"}\n```\nDone."))
	a := mustAnalyze(t, xWorkspace(), judge, &fakeBackend{})
	if reason := a.noImpact(t).Reason; !strings.Contains(reason, "fenced") {
		t.Fatalf("Reason = %q, want it to carry the reason fenced", reason)
	}
}

func TestStart_UnparseableTriageReplyErrorsWithReply(t *testing.T) {
	t.Parallel()

	a := analyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, text("I cannot decide.")), &fakeBackend{})
	if a.err == nil || !strings.Contains(a.err.Error(), "I cannot decide.") {
		t.Fatalf("Start() = %v, want an error quoting the reply", a.err)
	}
	if got := a.failure(t).Cause; got != review.CauseProvider {
		t.Errorf("Cause = %q, want %q", got, review.CauseProvider)
	}
}

func TestStart_TriageReplyWithoutImpactedErrors(t *testing.T) {
	t.Parallel()

	a := analyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, text(`{"reason": "hmm"}`)), &fakeBackend{})
	if a.err == nil || !strings.Contains(a.err.Error(), "impacted") || !strings.Contains(a.err.Error(), "hmm") {
		t.Fatalf("Start() = %v, want an error naming the missing field and quoting the reply", a.err)
	}
}

func TestStart_PromptsFencePatchAndMarkOmittedPatch(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(false)).on(pipeline.KindNewDoc, newDoc(false)), &fakeBackend{},
		mainGoChange(), review.ChangedFile{Path: "big.bin"})
	first := a.judge.asked[0]
	patchAt := strings.Index(first.prompt, "func main() {}")
	open := strings.LastIndex(first.prompt[:patchAt], "<<<UNTRUSTED-")
	end := strings.Index(first.prompt[patchAt:], "<<<END-")
	if open < 0 || end < 0 {
		t.Errorf("triage prompt does not fence the patch:\n%s", first.prompt)
	}
	if !strings.Contains(first.prompt, "(patch omitted by GitHub: large or binary file)") {
		t.Errorf("triage prompt does not mark the omitted patch:\n%s", first.prompt)
	}
	if !strings.Contains(first.system, "<<<UNTRUSTED-") {
		t.Errorf("triage system prompt does not explain the markers: %q", first.system)
	}
}

func TestStart_DraftPromptListsHunkRanges(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{submit()}})
	if got := a.backend.tasks[0].Prompt; !strings.Contains(got, `"main.go": 1-3`) {
		t.Errorf("draft prompt = %q, want it to contain \"main.go: 1-3\"", got)
	}
}

func TestStart_VerificationDropsRejectedProposal(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true), verify(false)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2), proposalFor("docs/x.md", 3))}})
	if proposals := a.proposals(t, 1); proposals[0].Anchor.Line != 2 {
		t.Fatalf("Verdict = %#v, want exactly the first proposal", a.result.Verdict)
	}
	if a.result.Model != "draft-model" {
		t.Errorf("Model = %q, want the draft model", a.result.Model)
	}
}

func TestStart_VerificationRejectsAllIsNoImpact(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, text(`{"supported": false, "reason": "diff\nunrelated"}`)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2))}})
	reason := a.noImpact(t).Reason
	if strings.Contains(reason, "\n") || !strings.Contains(reason, "unrelated") {
		t.Errorf("Reason = %q, want one line carrying the verification reason", reason)
	}
	if a.result.Model != "triage-model" {
		t.Errorf("Model = %q, want triage-model", a.result.Model)
	}
}

func TestStart_VerifyReplyWithoutSupportedErrors(t *testing.T) {
	t.Parallel()

	a := analyze(t, xWorkspace(),
		newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, text(`{"reason": "hmm"}`)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2))}})
	if a.err == nil || !strings.Contains(a.err.Error(), "supported") || !strings.Contains(a.err.Error(), "hmm") {
		t.Fatalf("Start() = %v, want an error naming the missing field and quoting the reply", a.err)
	}
}

func TestStart_ReportsUsageAndProducingModel(t *testing.T) {
	t.Parallel()

	t.Run("proposals sum triage, draft and verify", func(t *testing.T) {
		t.Parallel()

		judge := newJudge().
			on(pipeline.KindTriage, triage(true).withTokens(review.Tokens{Input: 10, Output: 1, CacheRead: 2})).
			on(pipeline.KindVerify, verify(true).withTokens(review.Tokens{Input: 7, Output: 1}))
		backend := &fakeBackend{
			submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2))},
			charge:      review.Tokens{Input: 20, Output: 5, CacheWrite: 3},
		}
		a := mustAnalyze(t, xWorkspace(), judge, backend)
		want := &review.Usage{Tokens: &review.Tokens{Input: 37, Output: 7, CacheRead: 2, CacheWrite: 3}}
		if diff := cmp.Diff(want, a.result.Usage); diff != "" {
			t.Errorf("Usage (-want +got):\n%s", diff)
		}
		if a.result.Model != "draft-model" {
			t.Errorf("Model = %q, want draft-model", a.result.Model)
		}
	})

	t.Run("a triage-only verdict names the triage model", func(t *testing.T) {
		t.Parallel()

		a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(false).withTokens(review.Tokens{Input: 4, Output: 2})), &fakeBackend{})
		if a.result.Model != "triage-model" {
			t.Errorf("Model = %q, want triage-model", a.result.Model)
		}
		if diff := cmp.Diff(&review.Usage{Tokens: &review.Tokens{Input: 4, Output: 2}}, a.result.Usage); diff != "" {
			t.Errorf("Usage (-want +got):\n%s", diff)
		}
	})

	t.Run("a run that called no model reports no usage", func(t *testing.T) {
		t.Parallel()

		a := mustAnalyze(t, xWorkspace(), newJudge(), &fakeBackend{}, review.ChangedFile{Path: "gone.go", Removed: true})
		if a.result.Usage != nil || a.result.Model != "" {
			t.Errorf("Usage, Model = %v, %q; want none", a.result.Usage, a.result.Model)
		}
	})
}

func TestMeter_ChargeFailsPastTheCap(t *testing.T) {
	t.Parallel()

	m := pipeline.NewMeter(10)
	if err := m.Charge(review.Tokens{Input: 6, Output: 4}); err != nil {
		t.Fatalf("Charge(10 of 10) = %v, want nil", err)
	}
	if err := m.Charge(review.Tokens{Input: 1}); err == nil {
		t.Fatal("Charge(11 of 10) = nil, want an error")
	}
}

func TestStart_NewDocPromptNotesAnUnreadableReadme(t *testing.T) {
	t.Parallel()

	const note = "docs/README.md exists but could not be read"
	tests := []struct {
		name     string
		ws       *fakeWorkspace
		wantNote bool
	}{
		{name: "absent", ws: xWorkspace()},
		{name: "readable", ws: xWorkspace().headFile("docs/README.md", "- [X](x.md): about x.\n")},
		{name: "symlink", ws: xWorkspace().at("docs/README.md", nonRegular()), wantNote: true},
		{name: "under a symlinked docs dir", ws: xWorkspace().at("docs", nonRegular()).at("docs/README.md", unreadable(errors.New("path escapes from parent")))},
		{name: "oversized", ws: xWorkspace().headFile("docs/README.md", strings.Repeat("x", 2<<20)), wantNote: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := mustAnalyze(t, tc.ws, newJudge().on(pipeline.KindNewDoc, newDoc(false)), &fakeBackend{}, otherGoChange())
			prompts := a.judge.prompts(pipeline.KindNewDoc)
			if len(prompts) != 1 {
				t.Fatalf("new-doc questions = %d, want 1", len(prompts))
			}
			if got := strings.Contains(prompts[0], note); got != tc.wantNote {
				t.Errorf("new-doc prompt contains the README note = %t, want %t", got, tc.wantNote)
			}
		})
	}
}
