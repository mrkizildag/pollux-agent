package sqlite_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestDroppedProposalsSurviveRestartAndClear(t *testing.T) {
	t.Parallel()
	store, path := open(t)
	state := gate.PRState{Owner: "o", Repo: "r", Number: 1, Dropped: []review.DroppedProposal{{Index: 2, Reason: "anchor outside diff"}}}
	if err := store.SavePR(t.Context(), state, gate.History{}); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.LoadPR(t.Context(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Fatal(diff)
	}
	state.Dropped = nil
	if err := reopened.SavePR(t.Context(), state, gate.History{}); err != nil {
		t.Fatal(err)
	}
	got, err = reopened.LoadPR(t.Context(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Dropped) != 0 {
		t.Fatal(got.Dropped)
	}
}
