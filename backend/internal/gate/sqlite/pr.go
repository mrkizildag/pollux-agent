package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/gate"
)

// LoadPR returns the state most recently saved for owner/repo#number, or the
// zero-HeadSHA state (identity fields filled from the args) if it was never saved.
func (s *Store) LoadPR(ctx context.Context, owner, repo string, number int) (gate.PRState, error) {
	state := gate.PRState{Owner: owner, Repo: repo, Number: number}

	var run gate.AwaitingRun
	var deadline, startedAt string
	row := s.db.QueryRowContext(ctx,
		`SELECT installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline, run_base_sha, run_started_at, run_runner, summary_comment_id, head_ref, proposals_sha,
			fork, pending_skip_user, pending_skip_scope, skip_user, skip_scope, skip_reason, skip_head_sha, failure_cause, pending_apply, dropped_proposals
		FROM pull_requests WHERE owner = ? AND repo = ? AND number = ?`,
		owner, repo, number)

	var pending gate.SkipAsk
	var skip gate.Skip
	var pendingApply, dropped string
	if err := row.Scan(&state.InstallationID, &state.HeadSHA, &state.CheckRunID, &run.RunID, &run.Nonce, &deadline, &run.BaseSHA, &startedAt, &run.Runner, &state.SummaryCommentID, &state.HeadRef, &state.ProposalsSHA,
		&state.Fork, &pending.User, &pending.Scope, &skip.User, &skip.Scope, &skip.Reason, &skip.HeadSHA, &state.FailureCause, &pendingApply, &dropped); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return state, nil
		}
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: %w", owner, repo, number, err)
	}

	if err := json.Unmarshal([]byte(dropped), &state.Dropped); err != nil {
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse dropped proposals: %w", owner, repo, number, err)
	}

	if len(state.Dropped) == 0 {
		state.Dropped = nil
	}

	if pending.User != "" {
		state.PendingSkip = &pending
	}
	if skip.User != "" {
		state.Skip = &skip
	}

	if pendingApply != "" {
		state.PendingApply = &gate.PendingApply{}
		if err := json.Unmarshal([]byte(pendingApply), state.PendingApply); err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse pending apply: %w", owner, repo, number, err)
		}
	}

	if run.Nonce != "" {
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse run deadline %q: %w", owner, repo, number, deadline, err)
		}
		run.Deadline = parsed
		if startedAt != "" {
			if run.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt); err != nil {
				return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d: parse run start %q: %w", owner, repo, number, startedAt, err)
			}
		}
		state.Run = &run
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, doc_path, section, comment_id, comment_url, state, content, original, index_entry, applied_sha, reply_id FROM pr_proposals
		WHERE owner = ? AND repo = ? AND number = ? ORDER BY position`,
		owner, repo, number)
	if err != nil {
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var p gate.ProposalState
		if err := rows.Scan(&p.ID, &p.DocPath, &p.Section, &p.CommentID, &p.CommentURL, &p.State, &p.Content, &p.Original, &p.IndexEntry, &p.AppliedSHA, &p.ReplyID); err != nil {
			return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
		}
		state.Proposals = append(state.Proposals, p)
	}
	if err := rows.Err(); err != nil {
		return gate.PRState{}, fmt.Errorf("load pr %s/%s#%d proposals: %w", owner, repo, number, err)
	}

	return state, nil
}

// SavePR upserts state, keyed by owner/repo/number, replacing the PR's
// proposal rows, and records history, all in one transaction.
func (s *Store) SavePR(ctx context.Context, state gate.PRState, history gate.History) error {
	var run gate.AwaitingRun
	var deadline, startedAt string
	if state.Run != nil {
		run = *state.Run
		deadline = run.Deadline.UTC().Format(time.RFC3339Nano)
		if !run.StartedAt.IsZero() {
			startedAt = run.StartedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	var pending gate.SkipAsk
	if state.PendingSkip != nil {
		pending = *state.PendingSkip
	}
	var skip gate.Skip
	if state.Skip != nil {
		skip = *state.Skip
	}

	var pendingApply []byte
	if state.PendingApply != nil {
		var err error
		if pendingApply, err = json.Marshal(state.PendingApply); err != nil {
			return fmt.Errorf("save pr %s/%s#%d: encode pending apply: %w", state.Owner, state.Repo, state.Number, err)
		}
	}

	dropped, err := json.Marshal(state.Dropped)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: encode dropped proposals: %w", state.Owner, state.Repo, state.Number, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: begin: %w", state.Owner, state.Repo, state.Number, err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO pull_requests (owner, repo, number, installation_id, head_sha, check_run_id, run_id, run_nonce, run_deadline, run_base_sha, run_started_at, run_runner, summary_comment_id, head_ref, proposals_sha,
			fork, pending_skip_user, pending_skip_scope, skip_user, skip_scope, skip_reason, skip_head_sha, failure_cause, pending_apply, dropped_proposals)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo, number) DO UPDATE SET
			installation_id = excluded.installation_id,
			head_sha = excluded.head_sha,
			check_run_id = excluded.check_run_id,
			run_id = excluded.run_id,
			run_nonce = excluded.run_nonce,
			run_deadline = excluded.run_deadline,
			run_base_sha = excluded.run_base_sha,
			run_started_at = excluded.run_started_at,
			run_runner = excluded.run_runner,
			summary_comment_id = excluded.summary_comment_id,
			head_ref = excluded.head_ref,
			proposals_sha = excluded.proposals_sha,
			fork = excluded.fork,
			pending_skip_user = excluded.pending_skip_user,
			pending_skip_scope = excluded.pending_skip_scope,
			skip_user = excluded.skip_user,
			skip_scope = excluded.skip_scope,
			skip_reason = excluded.skip_reason,
			skip_head_sha = excluded.skip_head_sha,
			failure_cause = excluded.failure_cause,
			pending_apply = excluded.pending_apply,
			dropped_proposals = excluded.dropped_proposals`,
		state.Owner, state.Repo, state.Number, state.InstallationID, state.HeadSHA,
		state.CheckRunID, run.RunID, run.Nonce, deadline, run.BaseSHA, startedAt, run.Runner, state.SummaryCommentID, state.HeadRef, state.ProposalsSHA,
		state.Fork, pending.User, pending.Scope, skip.User, skip.Scope, skip.Reason, skip.HeadSHA, state.FailureCause, string(pendingApply), string(dropped))
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: %w", state.Owner, state.Repo, state.Number, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM pr_proposals WHERE owner = ? AND repo = ? AND number = ?`,
		state.Owner, state.Repo, state.Number); err != nil {
		return fmt.Errorf("save pr %s/%s#%d: clear proposals: %w", state.Owner, state.Repo, state.Number, err)
	}
	for i, p := range state.Proposals {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pr_proposals (owner, repo, number, position, id, doc_path, section, comment_id, comment_url, state,
				content, original, index_entry, applied_sha, reply_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			state.Owner, state.Repo, state.Number, i, p.ID, p.DocPath, p.Section, p.CommentID, p.CommentURL, p.State,
			p.Content, p.Original, p.IndexEntry, p.AppliedSHA, p.ReplyID)
		if err != nil {
			return fmt.Errorf("save pr %s/%s#%d: proposal %s: %w", state.Owner, state.Repo, state.Number, p.ID, err)
		}
	}

	for _, a := range history.Analyses {
		if err := upsertAnalysis(ctx, tx, state, a); err != nil {
			return err
		}
	}
	for _, e := range history.Events {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO pr_events (owner, repo, number, key, kind, actor, proposal_id, scope, reason, commit_sha, head_sha, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (owner, repo, number, key) DO UPDATE SET reason = excluded.reason`,
			state.Owner, state.Repo, state.Number, e.Key, e.Kind, e.Actor, e.ProposalID, e.Scope, e.Reason, e.CommitSHA, e.HeadSHA, now())
		if err != nil {
			return fmt.Errorf("save pr %s/%s#%d: event %s: %w", state.Owner, state.Repo, state.Number, e.Key, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("save pr %s/%s#%d: commit: %w", state.Owner, state.Repo, state.Number, err)
	}
	return nil
}

// upsertAnalysis writes a for the pull request state describes; a later save
// of the same run nonce replaces the earlier row.
func upsertAnalysis(ctx context.Context, tx *sql.Tx, state gate.PRState, a gate.Analysis) error {
	var input, output, cacheRead, cacheWrite sql.NullInt64
	var cost sql.NullFloat64
	var basis string
	if u := a.Usage; u != nil {
		if t := u.Tokens; t != nil {
			input, output = sql.NullInt64{Int64: t.Input, Valid: true}, sql.NullInt64{Int64: t.Output, Valid: true}
			cacheRead, cacheWrite = sql.NullInt64{Int64: t.CacheRead, Valid: true}, sql.NullInt64{Int64: t.CacheWrite, Valid: true}
		}
		if u.CostUSD != nil {
			cost = sql.NullFloat64{Float64: *u.CostUSD, Valid: true}
		}
		basis = u.CostBasis
	}
	var startedAt sql.NullString
	if !a.StartedAt.IsZero() {
		startedAt = sql.NullString{String: a.StartedAt.UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO analyses (owner, repo, number, run_nonce, head_sha, runner, model, verdict, reason, proposals, started_at, finished_at, run_id,
			input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, cost_basis)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner, repo, number, run_nonce) DO UPDATE SET
			head_sha = excluded.head_sha,
			runner = excluded.runner,
			model = excluded.model,
			verdict = excluded.verdict,
			reason = excluded.reason,
			proposals = excluded.proposals,
			started_at = excluded.started_at,
			finished_at = excluded.finished_at,
			run_id = excluded.run_id,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cache_read_tokens = excluded.cache_read_tokens,
			cache_write_tokens = excluded.cache_write_tokens,
			cost_usd = excluded.cost_usd,
			cost_basis = excluded.cost_basis`,
		state.Owner, state.Repo, state.Number, a.Nonce, a.HeadSHA, a.Runner, a.Model, a.Verdict, a.Reason, a.Proposals,
		startedAt, a.FinishedAt.UTC().Format(time.RFC3339Nano), a.RunID,
		input, output, cacheRead, cacheWrite, cost, basis)
	if err != nil {
		return fmt.Errorf("save pr %s/%s#%d: analysis %s: %w", state.Owner, state.Repo, state.Number, a.Nonce, err)
	}
	return nil
}

// PRForRun returns the pull request that owner/repo's analysis run runID was
// dispatched for, and false if no pull request awaits it.
func (s *Store) PRForRun(ctx context.Context, owner, repo string, runID int64) (int, bool, error) {
	if runID == 0 {
		return 0, false, nil
	}

	var number int
	err := s.db.QueryRowContext(ctx,
		`SELECT number FROM pull_requests WHERE owner = ? AND repo = ? AND run_id = ?`,
		owner, repo, runID).Scan(&number)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find pr for run %d of %s/%s: %w", runID, owner, repo, err)
	}
	return number, true, nil
}

// PRsForHead returns the numbers of owner/repo's stored pull requests whose
// head is headSHA.
func (s *Store) PRsForHead(ctx context.Context, owner, repo, headSHA string) ([]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT number FROM pull_requests WHERE owner = ? AND repo = ? AND head_sha = ? ORDER BY number`,
		owner, repo, headSHA)
	if err != nil {
		return nil, fmt.Errorf("find prs for head %s of %s/%s: %w", headSHA, owner, repo, err)
	}
	defer func() { _ = rows.Close() }()

	var numbers []int
	for rows.Next() {
		var number int
		if err := rows.Scan(&number); err != nil {
			return nil, fmt.Errorf("scan pr for head %s of %s/%s: %w", headSHA, owner, repo, err)
		}
		numbers = append(numbers, number)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find prs for head %s of %s/%s: %w", headSHA, owner, repo, err)
	}
	return numbers, nil
}

// OverdueRuns returns the awaited runs, of pull requests and of scaffolds,
// whose deadline is before now. Deadlines are compared as times, not as text,
// because RFC3339Nano does not sort.
func (s *Store) OverdueRuns(ctx context.Context, now time.Time) ([]gate.OverdueRun, error) {
	prs, err := s.overdue(ctx, now, false,
		`SELECT owner, repo, number, run_nonce, run_deadline FROM pull_requests WHERE run_nonce != ''`)
	if err != nil {
		return nil, err
	}
	scaffolds, err := s.overdue(ctx, now, true,
		`SELECT owner, repo, 0, run_nonce, run_deadline FROM repo_scaffolds WHERE phase = 'awaiting' AND run_nonce != ''`)
	if err != nil {
		return nil, err
	}
	return append(prs, scaffolds...), nil
}

func (s *Store) overdue(ctx context.Context, now time.Time, scaffold bool, query string) ([]gate.OverdueRun, error) {
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list awaited runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var overdue []gate.OverdueRun
	for rows.Next() {
		run := gate.OverdueRun{Scaffold: scaffold}
		var deadline string
		if err := rows.Scan(&run.Owner, &run.Repo, &run.Number, &run.Nonce, &deadline); err != nil {
			return nil, fmt.Errorf("scan awaited run: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, deadline)
		if err != nil {
			return nil, fmt.Errorf("parse run deadline %q of %s/%s#%d: %w", deadline, run.Owner, run.Repo, run.Number, err)
		}
		if now.After(parsed) {
			run.Deadline = parsed
			overdue = append(overdue, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list awaited runs: %w", err)
	}
	return overdue, nil
}
