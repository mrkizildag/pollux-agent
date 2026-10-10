package sqlite_test

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
	"github.com/mrkizildag/pollux-agent/backend/internal/gate/sqlite"
	"github.com/mrkizildag/pollux-agent/backend/internal/jobqueue"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func open(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open(%q) = %v, want nil error", path, err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() = %v, want nil error", err)
		}
	})
	return store, path
}

func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open(%q) = %v, want nil error", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpen_MigrationsIdempotentOnReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")

	s1, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("first Open(%q) = %v, want nil error", path, err)
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil error", err)
	}

	s2, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("second Open(%q) = %v, want nil error", path, err)
	}
	if err := s2.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil error", err)
	}
}

func TestOpen_RejectsANewerSchema(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	if _, err := rawDB(t, path).ExecContext(t.Context(), "PRAGMA user_version = 999"); err != nil {
		t.Fatalf("set user_version = %v, want nil error", err)
	}

	store, err := sqlite.Open(t.Context(), path)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open() = nil, want an error for a schema newer than the binary")
	}
	if !strings.Contains(err.Error(), "database schema version 999 is newer than this binary's") {
		t.Errorf("Open() = %v, want it to name the newer schema version", err)
	}
}

func TestLoadPR_Unseen(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	got, err := store.LoadPR(t.Context(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v, want nil error", err)
	}

	want := gate.PRState{Owner: "acme", Repo: "widgets", Number: 7}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("LoadPR() mismatch (-want +got):\n%s", diff)
	}
}

func TestSavePR_RoundTripAndOverwrite(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	if err := store.SavePR(ctx, state, gate.History{}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}

	got, err := store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v, want nil error", err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Errorf("LoadPR() mismatch (-want +got):\n%s", diff)
	}

	overwrite := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha2"}
	if err := store.SavePR(ctx, overwrite, gate.History{}); err != nil {
		t.Fatalf("SavePR() overwrite = %v, want nil error", err)
	}

	got, err = store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() after overwrite = %v, want nil error", err)
	}
	if diff := cmp.Diff(overwrite, got); diff != "" {
		t.Errorf("LoadPR() after overwrite mismatch (-want +got):\n%s", diff)
	}
}

func TestSavePR_RoundTripRunAndProposalsAndPRForRun(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	state := gate.PRState{
		InstallationID:   1,
		Owner:            "acme",
		Repo:             "widgets",
		Number:           7,
		HeadSHA:          "sha1",
		CheckRunID:       555,
		HeadRef:          "feature",
		ProposalsSHA:     "sha1",
		Run:              &gate.AwaitingRun{RunID: 99, Nonce: "n1", Deadline: time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC), BaseSHA: "mb1"},
		SummaryCommentID: 99,
		Proposals: []gate.ProposalState{
			{ID: "aaa", DocPath: "docs/a.md", Section: "Usage", CommentID: 11, CommentURL: "https://x/11", State: gate.ProposalOpen},
			{ID: "bbb", DocPath: "docs/new.md", CommentID: 12, CommentURL: "https://x/12", State: gate.ProposalOutdated},
			{
				ID: "ccc", DocPath: "docs/c.md", Section: "C", CommentID: 13, CommentURL: "https://x/13", State: gate.ProposalApplied,
				Content: "## C\nnew\n", Original: "## C\nold\n", IndexEntry: "- [C](c.md)", AppliedSHA: "abc123", ReplyID: 77,
			},
		},
	}
	if err := store.SavePR(ctx, state, gate.History{}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}

	got, err := store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v, want nil error", err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Errorf("LoadPR() mismatch (-want +got):\n%s", diff)
	}

	number, ok, err := store.PRForRun(ctx, "acme", "widgets", 99)
	if err != nil || !ok || number != 7 {
		t.Errorf("PRForRun(run 99) = %d, %v, %v, want 7, true, nil", number, ok, err)
	}
	if _, ok, err := store.PRForRun(ctx, "acme", "widgets", 100); err != nil || ok {
		t.Errorf("PRForRun(unknown run) = %v, %v, want false, nil", ok, err)
	}
	if _, ok, err := store.PRForRun(ctx, "other", "widgets", 99); err != nil || ok {
		t.Errorf("PRForRun(other repo) = %v, %v, want false, nil", ok, err)
	}

	state.Run = nil
	if err := store.SavePR(ctx, state, gate.History{}); err != nil {
		t.Fatalf("SavePR() clearing run = %v, want nil error", err)
	}
	if _, ok, err := store.PRForRun(ctx, "acme", "widgets", 99); err != nil || ok {
		t.Errorf("PRForRun(cleared run) = %v, %v, want false, nil", ok, err)
	}

	state.Proposals = state.Proposals[1:]
	if err := store.SavePR(ctx, state, gate.History{}); err != nil {
		t.Fatalf("SavePR() replace = %v, want nil error", err)
	}
	got, err = store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() after replace = %v, want nil error", err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Errorf("LoadPR() after replace mismatch (-want +got):\n%s", diff)
	}
}

func TestSavePR_RoundTripForkAndSkips(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(*gate.PRState)
	}{
		{"none", func(*gate.PRState) {}},
		{"fork", func(s *gate.PRState) { s.Fork = true }},
		{"failure cause", func(s *gate.PRState) { s.FailureCause = "The analysis timed out." }},
		{"pending apply", func(s *gate.PRState) {
			s.PendingApply = &gate.PendingApply{IDs: []string{"p1", "p2"}, Message: "docs: apply 2 pollux-agent proposals", Parent: "sha0"}
		}},
		{"pending skip", func(s *gate.PRState) { s.PendingSkip = &gate.SkipAsk{User: "alice", Scope: gate.SkipPR} }},
		{"skip", func(s *gate.PRState) {
			s.Skip = &gate.Skip{User: "bob", Scope: gate.SkipCommit, Reason: "typo only", HeadSHA: "sha1"}
		}},
		{"all", func(s *gate.PRState) {
			s.Fork = true
			s.PendingSkip = &gate.SkipAsk{User: "alice", Scope: gate.SkipCommit}
			s.Skip = &gate.Skip{User: "bob", Scope: gate.SkipPR, Reason: "generated", HeadSHA: "sha2"}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, _ := open(t)
			ctx := t.Context()
			state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
			tc.edit(&state)

			if err := store.SavePR(ctx, state, gate.History{}); err != nil {
				t.Fatalf("SavePR() = %v, want nil error", err)
			}
			got, err := store.LoadPR(ctx, "acme", "widgets", 7)
			if err != nil {
				t.Fatalf("LoadPR() = %v, want nil error", err)
			}
			if diff := cmp.Diff(state, got); diff != "" {
				t.Errorf("LoadPR() mismatch (-want +got):\n%s", diff)
			}

			cleared := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
			if err := store.SavePR(ctx, cleared, gate.History{}); err != nil {
				t.Fatalf("SavePR(cleared) = %v, want nil error", err)
			}
			got, err = store.LoadPR(ctx, "acme", "widgets", 7)
			if err != nil {
				t.Fatalf("LoadPR() after clear = %v, want nil error", err)
			}
			if diff := cmp.Diff(cleared, got); diff != "" {
				t.Errorf("LoadPR() after clear mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestEnqueue_DuplicateDeliveryIsNoOp(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	job := jobqueue.NewJob{DeliveryID: "d1", Key: "owner/repo#1", Kind: "pull_request", Payload: []byte("a")}

	ok, superseded, err := store.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("Enqueue() first = %v, want nil error", err)
	}
	if !ok || superseded != nil {
		t.Fatalf("Enqueue() first = (%v, %v), want (true, nil)", ok, superseded)
	}

	ok, superseded, err = store.Enqueue(ctx, job)
	if err != nil {
		t.Fatalf("Enqueue() duplicate = %v, want nil error", err)
	}
	if ok || superseded != nil {
		t.Fatalf("Enqueue() duplicate = (%v, %v), want (false, nil)", ok, superseded)
	}

	count := 0
	for {
		_, gotOK, err := store.Claim(ctx)
		if err != nil {
			t.Fatalf("Claim() = %v, want nil error", err)
		}
		if !gotOK {
			break
		}
		count++
	}
	if count != 1 {
		t.Errorf("claimable job count = %d, want 1", count)
	}
}

func TestEnqueue_RedeliveryAfterFailedJobEnqueuesNewJob(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)

	first, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", first, ok, err)
	}
	if err := store.Finish(ctx, first.ID, jobqueue.StateFailed, "boom"); err != nil {
		t.Fatalf("Finish(%d, failed) = %v, want nil error", first.ID, err)
	}

	ok, superseded, err := store.Enqueue(ctx, jobqueue.NewJob{
		DeliveryID: "d1", Key: "owner/repo#1", Kind: "pull_request", Payload: []byte("payload"),
	})
	if err != nil {
		t.Fatalf("Enqueue() redelivery = %v, want nil error", err)
	}
	if !ok || superseded != nil {
		t.Fatalf("Enqueue() redelivery = (%v, %v), want (true, nil)", ok, superseded)
	}

	second, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() after redelivery = (%+v, %v, %v), want ok", second, ok, err)
	}
	if second.ID == first.ID {
		t.Errorf("Claim() after redelivery returned the same job id %d, want a new job", second.ID)
	}
}

func TestEnqueue_RedeliveryAfterNonFailedJobStaysNoOp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state jobqueue.State
	}{
		{"done", jobqueue.StateDone},
		{"superseded", jobqueue.StateSuperseded},
		{"pending", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store, _ := open(t)
			ctx := t.Context()

			enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)

			if tc.state != "" {
				claimed, ok, err := store.Claim(ctx)
				if err != nil || !ok {
					t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
				}
				if err := store.Finish(ctx, claimed.ID, tc.state, ""); err != nil {
					t.Fatalf("Finish(%d, %s) = %v, want nil error", claimed.ID, tc.state, err)
				}
			}

			ok, superseded, err := store.Enqueue(ctx, jobqueue.NewJob{
				DeliveryID: "d1", Key: "owner/repo#1", Kind: "pull_request", Payload: []byte("payload"),
			})
			if err != nil {
				t.Fatalf("Enqueue() redelivery = %v, want nil error", err)
			}
			if ok || superseded != nil {
				t.Fatalf("Enqueue() redelivery = (%v, %v), want (false, nil)", ok, superseded)
			}
		})
	}
}

func TestClaim_OrderAndPerKeyExclusion(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)
	enqueue(t, store, "d2", "owner/repo#1", "pull_request", false)
	enqueue(t, store, "d3", "owner/repo#2", "pull_request", false)

	first, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() first = (%+v, %v, %v), want ok", first, ok, err)
	}
	if first.Key != "owner/repo#1" {
		t.Fatalf("Claim() first key = %q, want owner/repo#1", first.Key)
	}

	other, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() other key = (%+v, %v, %v), want ok", other, ok, err)
	}
	if other.Key != "owner/repo#2" {
		t.Fatalf("Claim() other key = %q, want owner/repo#2", other.Key)
	}

	_, ok, err = store.Claim(ctx)
	if err != nil {
		t.Fatalf("Claim() while same-key job running = %v, want nil error", err)
	}
	if ok {
		t.Fatalf("Claim() while same-key job running returned ok, want false")
	}

	if err := store.Finish(ctx, first.ID, jobqueue.StateDone, ""); err != nil {
		t.Fatalf("Finish(%d) = %v, want nil error", first.ID, err)
	}

	second, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() after Finish = (%+v, %v, %v), want ok", second, ok, err)
	}
	if second.Key != "owner/repo#1" {
		t.Fatalf("Claim() after Finish key = %q, want owner/repo#1", second.Key)
	}
}

func TestEnqueue_DifferentKindNotSuperseded(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "sync", false)

	superseded := enqueueWithResult(t, store, "d2", "owner/repo#1", "other", true)
	if superseded != nil {
		t.Errorf("Enqueue(other kind) superseded = %v, want nil", superseded)
	}

	// If the first job had been superseded, it would no longer be claimable
	// and the "other" kind job would be claimed instead.
	claimed, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
	}
	if claimed.Kind != "sync" {
		t.Errorf("Claim() kind = %q, want sync (original job must not be superseded by a different kind)", claimed.Kind)
	}
}

func TestEnqueue_SupersedesPending(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "sync", false)

	superseded := enqueueWithResult(t, store, "d2", "owner/repo#1", "sync", true)
	if superseded != nil {
		t.Errorf("Enqueue(supersedes pending) running IDs = %v, want nil", superseded)
	}

	claimed, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
	}
	if claimed.Key != "owner/repo#1" || claimed.Kind != "sync" {
		t.Errorf("Claim() = %+v, want the superseding job", claimed)
	}

	_, ok, err = store.Claim(ctx)
	if err != nil {
		t.Fatalf("second Claim() = %v, want nil error", err)
	}
	if ok {
		t.Errorf("second Claim() = ok, want false: the superseded pending job must not be claimable")
	}
}

func TestEnqueue_SupersedesRunning(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "sync", false)

	running, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", running, ok, err)
	}

	supersededRunning := enqueueWithResult(t, store, "d2", "owner/repo#1", "sync", true)
	if diff := cmp.Diff([]int64{running.ID}, supersededRunning); diff != "" {
		t.Errorf("Enqueue(supersedes running) mismatch (-want +got):\n%s", diff)
	}
}

func TestRequeueRunning(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)

	claimed, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
	}

	n, err := store.RequeueRunning(ctx)
	if err != nil {
		t.Fatalf("RequeueRunning() = %v, want nil error", err)
	}
	if n != 1 {
		t.Fatalf("RequeueRunning() = %d, want 1", n)
	}

	again, ok, err := store.Claim(ctx)
	if err != nil || !ok {
		t.Fatalf("Claim() after requeue = (%+v, %v, %v), want ok", again, ok, err)
	}
	if again.ID != claimed.ID {
		t.Errorf("Claim() after requeue id = %d, want %d", again.ID, claimed.ID)
	}
}

func finishNext(t *testing.T, store *sqlite.Store, state jobqueue.State) {
	t.Helper()
	claimed, ok, err := store.Claim(t.Context())
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
	}
	if err := store.Finish(t.Context(), claimed.ID, state, ""); err != nil {
		t.Fatalf("Finish(%d, %s) = %v, want nil error", claimed.ID, state, err)
	}
}

func prune(t *testing.T, store *sqlite.Store, before time.Time) (jobs, deliveries int) {
	t.Helper()
	jobs, deliveries, err := store.Prune(t.Context(), before)
	if err != nil {
		t.Fatalf("Prune(%v) = %v, want nil error", before, err)
	}
	return jobs, deliveries
}

func TestPrune_FinishedJobsAndTheirDeliveriesByAge(t *testing.T) {
	t.Parallel()

	for _, state := range []jobqueue.State{jobqueue.StateDone, jobqueue.StateFailed, jobqueue.StateSuperseded} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()

			store, _ := open(t)

			enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)
			finishNext(t, store, state)

			if j, d := prune(t, store, time.Now().Add(-time.Hour)); j != 0 || d != 0 {
				t.Errorf("Prune(recent cutoff) = (%d, %d), want (0, 0)", j, d)
			}
			if j, d := prune(t, store, time.Now().Add(time.Hour)); j != 1 || d != 1 {
				t.Errorf("Prune(old cutoff) = (%d, %d), want (1, 1)", j, d)
			}
			if j, d := prune(t, store, time.Now().Add(time.Hour)); j != 0 || d != 0 {
				t.Errorf("second Prune(old cutoff) = (%d, %d), want (0, 0)", j, d)
			}
		})
	}
}

func TestPrune_KeepsPendingAndRunningJobsAndTheirDeliveries(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	enqueue(t, store, "running", "owner/repo#1", "pull_request", false)
	enqueue(t, store, "pending", "owner/repo#1", "pull_request", false)
	claimed, ok, err := store.Claim(t.Context())
	if err != nil || !ok {
		t.Fatalf("Claim() = (%+v, %v, %v), want ok", claimed, ok, err)
	}

	if j, d := prune(t, store, time.Now().Add(time.Hour)); j != 0 || d != 0 {
		t.Errorf("Prune(old cutoff) = (%d, %d), want (0, 0)", j, d)
	}
	if ok, _ := enqueueJob(t, store, "running", "owner/repo#1", "pull_request", false); ok {
		t.Errorf("Enqueue(running) after prune = true, want false")
	}
	if ok, _ := enqueueJob(t, store, "pending", "owner/repo#1", "pull_request", false); ok {
		t.Errorf("Enqueue(pending) after prune = true, want false")
	}
}

func TestPrune_RecentCutoffKeepsDedupAndFailedRetry(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	enqueue(t, store, "done", "owner/repo#1", "pull_request", false)
	finishNext(t, store, jobqueue.StateDone)
	enqueue(t, store, "failed", "owner/repo#2", "pull_request", false)
	finishNext(t, store, jobqueue.StateFailed)

	if j, d := prune(t, store, time.Now().Add(-time.Hour)); j != 0 || d != 0 {
		t.Fatalf("Prune(recent cutoff) = (%d, %d), want (0, 0)", j, d)
	}
	if ok, _ := enqueueJob(t, store, "done", "owner/repo#1", "pull_request", false); ok {
		t.Errorf("Enqueue(done) after prune = true, want false")
	}
	if ok, _ := enqueueJob(t, store, "failed", "owner/repo#2", "pull_request", false); !ok {
		t.Errorf("Enqueue(failed) after prune = false, want true")
	}
}

func TestPrune_RedeliveryAfterPruneRunsAgain(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)
	finishNext(t, store, jobqueue.StateDone)

	if j, d := prune(t, store, time.Now().Add(time.Hour)); j != 1 || d != 1 {
		t.Fatalf("Prune(old cutoff) = (%d, %d), want (1, 1)", j, d)
	}
	if ok, _ := enqueueJob(t, store, "d1", "owner/repo#1", "pull_request", false); !ok {
		t.Errorf("Enqueue(d1) after prune = false, want true")
	}
}

func TestPrune_KeepsDeliveryWhoseRedeliveredJobIsPending(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)
	finishNext(t, store, jobqueue.StateFailed)
	if ok, _ := enqueueJob(t, store, "d1", "owner/repo#1", "pull_request", false); !ok {
		t.Fatalf("Enqueue(d1) redelivery = false, want true")
	}

	if j, d := prune(t, store, time.Now().Add(time.Hour)); j != 1 || d != 0 {
		t.Errorf("Prune(old cutoff) = (%d, %d), want (1, 0)", j, d)
	}
	if ok, _ := enqueueJob(t, store, "d1", "owner/repo#1", "pull_request", false); ok {
		t.Errorf("Enqueue(d1) while its job is pending = true, want false")
	}
}

func TestPrune_OldFailedJobGoneButLaterDoneJobKeepsDeliveryDeduped(t *testing.T) {
	t.Parallel()

	store, _ := open(t)

	enqueue(t, store, "d1", "owner/repo#1", "pull_request", false)
	finishNext(t, store, jobqueue.StateFailed)
	time.Sleep(5 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(5 * time.Millisecond)
	if ok, _ := enqueueJob(t, store, "d1", "owner/repo#1", "pull_request", false); !ok {
		t.Fatalf("Enqueue(d1) redelivery = false, want true")
	}
	finishNext(t, store, jobqueue.StateDone)

	if j, d := prune(t, store, cutoff); j != 1 || d != 0 {
		t.Errorf("Prune(between failed and done) = (%d, %d), want (1, 0)", j, d)
	}
	if ok, _ := enqueueJob(t, store, "d1", "owner/repo#1", "pull_request", false); ok {
		t.Errorf("Enqueue(d1) after its failed job was pruned and done job kept = true, want false")
	}
}

func TestData_SurvivesCloseAndOpen(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()

	if err := store.SavePR(ctx, gate.PRState{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}, gate.History{}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil error", err)
	}

	reopened, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatalf("Open(%q) = %v, want nil error", path, err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close() = %v, want nil error", err)
		}
	})

	got, err := reopened.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v, want nil error", err)
	}
	want := gate.PRState{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("LoadPR() after reopen mismatch (-want +got):\n%s", diff)
	}
}

func enqueueJob(t *testing.T, store *sqlite.Store, deliveryID, key, kind string, supersedes bool) (bool, []int64) {
	t.Helper()
	ok, superseded, err := store.Enqueue(t.Context(), jobqueue.NewJob{
		DeliveryID: deliveryID,
		Key:        key,
		Kind:       kind,
		Payload:    []byte("payload"),
		Supersedes: supersedes,
	})
	if err != nil {
		t.Fatalf("Enqueue(%q) = %v, want nil error", deliveryID, err)
	}
	return ok, superseded
}

func enqueue(t *testing.T, store *sqlite.Store, deliveryID, key, kind string, supersedes bool) {
	t.Helper()
	if ok, _ := enqueueJob(t, store, deliveryID, key, kind, supersedes); !ok {
		t.Fatalf("Enqueue(%q) = false, want true", deliveryID)
	}
}

func enqueueWithResult(t *testing.T, store *sqlite.Store, deliveryID, key, kind string, supersedes bool) []int64 {
	t.Helper()
	ok, superseded := enqueueJob(t, store, deliveryID, key, kind, supersedes)
	if !ok {
		t.Fatalf("Enqueue(%q) = false, want true", deliveryID)
	}
	return superseded
}

func TestOverdueRuns(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	for _, st := range []gate.PRState{
		{Owner: "acme", Repo: "widgets", Number: 1, HeadSHA: "a", CheckRunID: 1, Run: &gate.AwaitingRun{RunID: 1, Nonce: "n1", Deadline: base}},
		{Owner: "acme", Repo: "widgets", Number: 2, HeadSHA: "b", CheckRunID: 2, Run: &gate.AwaitingRun{RunID: 2, Nonce: "n2", Deadline: base.Add(time.Hour)}},
		{Owner: "acme", Repo: "widgets", Number: 3, HeadSHA: "c"},
	} {
		if err := store.SavePR(ctx, st, gate.History{}); err != nil {
			t.Fatalf("SavePR(%+v) = %v, want nil error", st, err)
		}
	}

	got, err := store.OverdueRuns(ctx, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("OverdueRuns() = %v, want nil error", err)
	}
	want := []gate.OverdueRun{{PRRef: gate.PRRef{Owner: "acme", Repo: "widgets", Number: 1}, Nonce: "n1", Deadline: base}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("OverdueRuns() mismatch (-want +got):\n%s", diff)
	}
}

func TestPRsForHead(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()
	for _, st := range []gate.PRState{
		{Owner: "acme", Repo: "widgets", Number: 8, HeadSHA: "a"},
		{Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "a"},
		{Owner: "acme", Repo: "widgets", Number: 9, HeadSHA: "b"},
		{Owner: "acme", Repo: "other", Number: 1, HeadSHA: "a"},
	} {
		if err := store.SavePR(ctx, st, gate.History{}); err != nil {
			t.Fatalf("SavePR(%+v) = %v, want nil error", st, err)
		}
	}

	got, err := store.PRsForHead(ctx, "acme", "widgets", "a")
	if err != nil {
		t.Fatalf("PRsForHead() = %v, want nil error", err)
	}
	if diff := cmp.Diff([]int{7, 8}, got); diff != "" {
		t.Errorf("PRsForHead() mismatch (-want +got):\n%s", diff)
	}
	if got, err := store.PRsForHead(ctx, "acme", "widgets", "zzz"); err != nil || len(got) != 0 {
		t.Errorf("PRsForHead(unknown head) = %v, %v, want none", got, err)
	}
}

func countAnalyses(t *testing.T, path string) (n int, verdict gate.AnalysisVerdict) {
	t.Helper()
	if err := rawDB(t, path).QueryRowContext(t.Context(), `SELECT COUNT(*), COALESCE(MAX(verdict), '') FROM analyses`).Scan(&n, &verdict); err != nil {
		t.Fatalf("query analyses = %v, want nil error", err)
	}
	return n, verdict
}

func TestSavePR_AnalysisUpsertsPerNonceLastWins(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	analysis := &gate.Analysis{Nonce: "check-1", HeadSHA: "sha1", Runner: gate.RunnerKindServer, Verdict: gate.VerdictProposals, Proposals: 2, StartedAt: at, FinishedAt: at}
	if err := store.SavePR(ctx, state, gate.History{Analyses: []gate.Analysis{*analysis}}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}
	analysis.Verdict, analysis.Proposals = gate.VerdictFailed, 0
	if err := store.SavePR(ctx, state, gate.History{Analyses: []gate.Analysis{*analysis}}); err != nil {
		t.Fatalf("SavePR() again = %v, want nil error", err)
	}
	if n, verdict := countAnalyses(t, path); n != 1 || verdict != gate.VerdictFailed {
		t.Errorf("analyses = %d rows with verdict %q, want 1 row with %q", n, verdict, gate.VerdictFailed)
	}

	analysis.Nonce = "check-2"
	if err := store.SavePR(ctx, state, gate.History{Analyses: []gate.Analysis{*analysis}}); err != nil {
		t.Fatalf("SavePR() new nonce = %v, want nil error", err)
	}
	if n, _ := countAnalyses(t, path); n != 2 {
		t.Errorf("analyses after a new nonce = %d rows, want 2", n)
	}
}

func TestSavePR_RoundTripsRunStartAndRunner(t *testing.T) {
	t.Parallel()

	store, _ := open(t)
	ctx := t.Context()

	started := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1",
		Run: &gate.AwaitingRun{Nonce: "n", Deadline: started.Add(time.Minute), StartedAt: started, Runner: gate.RunnerKindActions}}
	if err := store.SavePR(ctx, state, gate.History{}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}
	got, err := store.LoadPR(ctx, "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("LoadPR() = %v, want nil error", err)
	}
	if diff := cmp.Diff(state, got); diff != "" {
		t.Errorf("LoadPR() mismatch (-want +got):\n%s", diff)
	}
}

func TestOpen_BackfillsArmedRunsWhenMigratingToAnalysisHistory(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "state.db")
	seeded, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open(%q) = %v, want nil error", path, err)
	}
	if err := seeded.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil error", err)
	}
	db := rawDB(t, path)
	if _, err := db.ExecContext(t.Context(), `DROP TABLE analyses; DROP TABLE pr_events; DROP TABLE login_attempts; DROP TABLE sessions;
		ALTER TABLE pull_requests DROP COLUMN run_started_at;
		ALTER TABLE pull_requests DROP COLUMN run_runner;
		ALTER TABLE pull_requests DROP COLUMN dropped_proposals;
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha) VALUES ('acme', 'widgets', 7, 1, 'sha0');
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha, run_id, run_nonce, run_deadline) VALUES ('acme', 'widgets', 8, 1, 'sha0', 99, 'n-actions', '2026-01-02T03:04:05Z');
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha, run_id, run_nonce, run_deadline) VALUES ('acme', 'widgets', 9, 1, 'sha0', 0, 'n-unknown', '2026-01-02T03:04:05Z');
		PRAGMA user_version = 10`); err != nil {
		t.Fatalf("seed pre-history database = %v, want nil error", err)
	}

	store, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatalf("Open() pre-history database = %v, want nil error", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if n, _ := countAnalyses(t, path); n != 0 {
		t.Errorf("analyses after migration = %d rows, want 0", n)
	}

	for number, want := range map[int]gate.RunnerKind{8: gate.RunnerKindActions, 9: ""} {
		state, err := store.LoadPR(t.Context(), "acme", "widgets", number)
		if err != nil {
			t.Fatalf("LoadPR(%d) = %v, want nil error", number, err)
		}
		if state.Run == nil || state.Run.Runner != want {
			t.Errorf("LoadPR(%d).Run = %+v, want runner %q", number, state.Run, want)
		}
		if len(state.Dropped) != 0 {
			t.Errorf("LoadPR(%d).Dropped = %+v, want no notice for a pre-history row", number, state.Dropped)
		}
	}
	if state, err := store.LoadPR(t.Context(), "acme", "widgets", 7); err != nil || state.Run != nil {
		t.Errorf("LoadPR(7) = %+v, %v, want no run", state.Run, err)
	}

	state, err := store.LoadPR(t.Context(), "acme", "widgets", 8)
	if err != nil {
		t.Fatalf("LoadPR(8) = %v, want nil error", err)
	}
	run := state.Run
	analysis := &gate.Analysis{Nonce: run.Nonce, HeadSHA: state.HeadSHA, Runner: run.Runner, Verdict: gate.VerdictNoImpact, StartedAt: run.StartedAt, FinishedAt: run.Deadline, RunID: run.RunID}
	state.Run = nil
	if err := store.SavePR(t.Context(), state, gate.History{Analyses: []gate.Analysis{*analysis}}); err != nil {
		t.Fatalf("SavePR() = %v, want nil error", err)
	}
	var runner string
	var startedAt sql.NullString
	if err := db.QueryRowContext(t.Context(), `SELECT runner, started_at FROM analyses WHERE run_nonce = 'n-actions'`).Scan(&runner, &startedAt); err != nil {
		t.Fatalf("query analysis = %v, want nil error", err)
	}
	if runner != string(gate.RunnerKindActions) || startedAt.Valid {
		t.Errorf("analysis runner, started_at = %q, %v, want %q, NULL", runner, startedAt, gate.RunnerKindActions)
	}
}

func TestSavePR_AnalysisUsageNullVersusValue(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()
	db := rawDB(t, path)

	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cost, half := 0.0, 0.5
	tests := []struct {
		name      string
		usage     *review.Usage
		wantIn    sql.NullInt64
		wantCache sql.NullInt64
		wantCost  sql.NullFloat64
		wantBasis string
	}{
		{name: "nil usage is NULL", usage: nil},
		{
			name:   "tokens without cost",
			usage:  &review.Usage{Tokens: &review.Tokens{Input: 10, Output: 20, CacheRead: 30, CacheWrite: 40}},
			wantIn: sql.NullInt64{Int64: 10, Valid: true}, wantCache: sql.NullInt64{Int64: 40, Valid: true},
		},
		{
			name:     "cost without tokens leaves tokens NULL",
			usage:    &review.Usage{CostUSD: &half, CostBasis: "list"},
			wantCost: sql.NullFloat64{Float64: 0.5, Valid: true}, wantBasis: "list",
		},
		{
			name:   "reported zero cost stays zero",
			usage:  &review.Usage{Tokens: &review.Tokens{Input: 1}, CostUSD: &cost, CostBasis: "list"},
			wantIn: sql.NullInt64{Int64: 1, Valid: true}, wantCache: sql.NullInt64{Valid: true},
			wantCost: sql.NullFloat64{Valid: true}, wantBasis: "list",
		},
	}
	for i, tt := range tests {
		nonce := fmt.Sprintf("check-%d", i)
		state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
		analysis := &gate.Analysis{Nonce: nonce, HeadSHA: "sha1", Runner: gate.RunnerKindServer, Verdict: gate.VerdictNoImpact, StartedAt: at, FinishedAt: at, Usage: tt.usage}
		if err := store.SavePR(ctx, state, gate.History{Analyses: []gate.Analysis{*analysis}}); err != nil {
			t.Fatalf("%s: SavePR() = %v, want nil error", tt.name, err)
		}

		var in, cacheWrite sql.NullInt64
		var gotCost sql.NullFloat64
		var basis string
		err := db.QueryRowContext(ctx, `SELECT input_tokens, cache_write_tokens, cost_usd, cost_basis FROM analyses WHERE run_nonce = ?`, nonce).
			Scan(&in, &cacheWrite, &gotCost, &basis)
		if err != nil {
			t.Fatalf("%s: query analyses = %v, want nil error", tt.name, err)
		}
		if in != tt.wantIn || cacheWrite != tt.wantCache || gotCost != tt.wantCost || basis != tt.wantBasis {
			t.Errorf("%s: input, cache_write, cost, basis = %v, %v, %v, %q, want %v, %v, %v, %q",
				tt.name, in, cacheWrite, gotCost, basis, tt.wantIn, tt.wantCache, tt.wantCost, tt.wantBasis)
		}
	}
}

func TestSavePR_EventsSavedTwiceWriteOnce(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()

	state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	history := gate.History{Events: []gate.PREvent{
		{Key: "outdated/p1/sha1", Kind: gate.EventOutdated, ProposalID: "p1", HeadSHA: "sha1"},
		{Key: "outdated/p2/sha1", Kind: gate.EventOutdated, ProposalID: "p2", HeadSHA: "sha1"},
	}}
	for range 2 {
		if err := store.SavePR(ctx, state, history); err != nil {
			t.Fatalf("SavePR() = %v, want nil error", err)
		}
	}

	var n int
	if err := rawDB(t, path).QueryRowContext(ctx, `SELECT COUNT(*) FROM pr_events`).Scan(&n); err != nil {
		t.Fatalf("query pr_events = %v, want nil error", err)
	}
	if n != 2 {
		t.Errorf("pr_events = %d rows after saving two events twice, want 2", n)
	}
}

func TestSavePR_SecondSkipKeepsTheNewReasonAndTheFirstTimestamp(t *testing.T) {
	t.Parallel()

	store, path := open(t)
	ctx := t.Context()
	db := rawDB(t, path)

	state := gate.PRState{InstallationID: 1, Owner: "acme", Repo: "widgets", Number: 7, HeadSHA: "sha1"}
	skip := gate.PREvent{Key: "skipped/alice/commit/sha1", Kind: gate.EventSkipped, Actor: "alice", Scope: gate.SkipCommit, Reason: "first", HeadSHA: "sha1"}
	var firstAt string
	for i, reason := range []string{"first", "second"} {
		skip.Reason = reason
		if err := store.SavePR(ctx, state, gate.History{Events: []gate.PREvent{skip}}); err != nil {
			t.Fatalf("SavePR(%q) = %v, want nil error", reason, err)
		}
		if i == 0 {
			if err := db.QueryRowContext(ctx, `SELECT created_at FROM pr_events`).Scan(&firstAt); err != nil {
				t.Fatalf("query created_at = %v, want nil error", err)
			}
			if _, err := time.Parse(time.RFC3339Nano, firstAt); err != nil {
				t.Fatalf("created_at %q is not RFC3339Nano: %v", firstAt, err)
			}
		}
	}

	var n int
	var reason, createdAt string
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), MAX(reason), MAX(created_at) FROM pr_events`).Scan(&n, &reason, &createdAt); err != nil {
		t.Fatalf("query pr_events = %v, want nil error", err)
	}
	if n != 1 || reason != "second" || createdAt != firstAt {
		t.Errorf("pr_events = %d rows, reason %q, created_at %q, want 1 row, %q, %q", n, reason, createdAt, "second", firstAt)
	}
}
