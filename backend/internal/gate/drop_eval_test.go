package gate_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestEvalNextPartialAnalysisReplacesNotice(t *testing.T) {
	t.Parallel()
	state := awaitingState()
	state.Dropped = []review.DroppedProposal{{Index: 1, Reason: "previous reason"}}
	store := &fakeStore{stored: state, live: true}
	gh := &fakeGitHub{}
	runner := &fakeRunner{result: review.Result{
		Verdict: review.Proposals{proposal("docs/a.md", "A")},
		Dropped: []review.DroppedProposal{{Index: 2, Reason: "new anchor reason"}},
	}}
	svc := gate.NewService(gh, nil, store, gate.Runners{Actions: runner}, nil, nil).WithCollectBackoff(0)
	if err := svc.HandleRunCompleted(t.Context(), completedRun("success")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{gh.updates[0].run.Summary, renderedSummary(t, store.stored)} {
		if strings.Contains(body, "previous reason") || strings.Count(body, "Dropped 1 proposal: new anchor reason") != 1 {
			t.Fatalf("next conclusion notice = %q", body)
		}
	}
}
