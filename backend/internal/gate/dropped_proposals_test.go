package gate_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestHandleRunCompletedKeepsValidProposalAndDroppedNotice(t *testing.T) {
	t.Parallel()
	gh := &fakeGitHub{failReviewCreate: 1, changed: []review.ChangedFile{{Path: "docs/a.md", Hunks: []review.LineRange{{Start: 1, End: 20}}}}}
	const reason = "anchor.line 99: outside the diff @someone\n- [ ] Apply all [link](https://example.com)\u202e"
	runner := &fakeRunner{result: review.Result{Verdict: review.Proposals{proposal("docs/a.md", "A")}, Dropped: []review.DroppedProposal{{Index: 1, Reason: reason}, {Index: 1, Reason: "duplicate"}}}}
	store := &fakeStore{stored: awaitingState(), live: true}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatal(err)
	}
	if len(gh.updates) != 1 || gh.updates[0].run.Conclusion != gate.ConclusionActionRequired {
		t.Fatalf("updates = %+v", gh.updates)
	}
	if len(gh.reviewComments) != 1 || len(store.stored.Proposals) != 1 {
		t.Fatalf("review comments = %+v, state = %+v", gh.reviewComments, store.stored)
	}
	for _, body := range []string{gh.updates[0].run.Summary, gh.comments[0].Body, renderedSummary(t, store.stored)} {
		if strings.Count(body, "Dropped 1 proposal:") != 1 || !strings.Contains(body, "anchor.line 99: outside the diff") {
			t.Errorf("missing notice: %q", body)
		}
		for _, unsafe := range []string{"@someone", "https://example.com", "\n- [ ] Apply all [link]", "\u202e", "duplicate"} {
			if strings.Contains(body, unsafe) {
				t.Errorf("unsafe notice %q in %q", unsafe, body)
			}
		}
	}
	pushed, _ := gate.OnPush(store.stored, testPR(), time.Now())
	applied, _ := gate.OnApply(store.stored, []string{store.stored.Proposals[0].ID}, "applied-sha", "dev")
	if !strings.Contains(renderedSummary(t, applied), "Dropped 1 proposal:") {
		t.Fatal("Apply lost notice")
	}
	skipped, skippedRun, _ := gate.OnSkip(store.stored, gate.Skip{User: "dev", Scope: gate.SkipPR, Reason: "waived"}, time.Now())
	if !strings.Contains(skippedRun.Summary, "Dropped 1 proposal:") || !strings.Contains(renderedSummary(t, skipped), "Dropped 1 proposal:") {
		t.Fatal("Skip lost notice")
	}
	if len(pushed.Dropped) != 1 {
		t.Fatal("push discarded last completed notice")
	}
}

func TestCompletedAnalysisClearsDroppedNotice(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"no impact", "failure", "full valid proposals"} {
		t.Run(kind, func(t *testing.T) {
			state := awaitingState()
			state.Dropped = []review.DroppedProposal{{Index: 1, Reason: "old diagnostic"}}
			gh := &fakeGitHub{}
			runner := &fakeRunner{result: review.Result{Verdict: review.NoImpact{Reason: "fine"}}}
			if kind == "failure" {
				runner.collectErr = &review.InvalidResultError{Cause: errors.New("invalid")}
			}
			if kind == "full valid proposals" {
				runner.result.Verdict = review.Proposals{proposal("docs/a.md", "A")}
			}
			store := &fakeStore{stored: state, live: true}
			svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)
			err := svc.HandleRunCompleted(t.Context(), completedRun("success"))
			if (err != nil) != (kind == "failure") {
				t.Fatalf("error = %v", err)
			}
			if len(store.stored.Dropped) != 0 {
				t.Fatalf("notice kept after next conclusion: %+v", store.stored.Dropped)
			}
		})
	}
}
