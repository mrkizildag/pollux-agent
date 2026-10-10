package gate

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// skipActive reports whether a skip covers the head state reports on.
func skipActive(s PRState) bool {
	return s.Skip != nil && (s.Skip.Scope == SkipPR || s.Skip.HeadSHA == s.HeadSHA)
}

// command is the one-step slash command for this scope.
func (sc SkipScope) command() string {
	if sc == SkipPR {
		return "/pollux-agent skip-pr"
	}
	return "/pollux-agent skip"
}

func (sc SkipScope) noun() string {
	if sc == SkipPR {
		return "PR"
	}
	return "commit"
}

// skipRun is the success check run for the active skip of s: pure, no I/O.
func skipRun(s PRState) CheckRun {
	sk := s.Skip
	return withDroppedNotice(CheckRun{
		Name:       CheckName,
		HeadSHA:    s.HeadSHA,
		Status:     StatusCompleted,
		Conclusion: ConclusionSuccess,
		Title:      fmt.Sprintf("Skipped by @%s", sk.User),
		Summary:    truncate(fmt.Sprintf("@%s skipped the docs check for this %s: %s", sk.User, sk.Scope.noun(), inertProse(sk.Reason)), maxSummaryBytes),
	}, s.Dropped)
}

// OnSkip is the state transition for a skip made at the current head at now:
// pure, no I/O. The skip wins over any awaited run, so that run's result is
// ignored and recorded as superseded. It emits a skipped event per open
// proposal, or one without a proposal when none is open.
func OnSkip(s PRState, sk Skip, now time.Time) (PRState, CheckRun, History) {
	s.Skip = &Skip{User: sk.User, Scope: sk.Scope, Reason: sk.Reason, HeadSHA: s.HeadSHA}
	s.PendingSkip = nil
	var history History
	if s.Run != nil {
		history.Analyses = []Analysis{*analysisBase(s.Run, s.HeadSHA, VerdictSuperseded, now)}
		s.Run = nil
	}
	for _, p := range s.Proposals {
		if p.State == ProposalOpen {
			history.Events = append(history.Events, skippedEvent(*s.Skip, p.ID))
		}
	}
	if len(history.Events) == 0 {
		history.Events = append(history.Events, skippedEvent(*s.Skip, ""))
	}
	return s, skipRun(s), history
}

// maxReasonRunes caps a skip reason, which is echoed in the summary and the check run.
const maxReasonRunes = 500

// cleanReason is a user-written skip reason as stored: one line, capped. Every
// render passes it through inertProse.
func cleanReason(reason string) string {
	reason = oneLine(reason)
	if r := []rune(reason); len(r) > maxReasonRunes {
		reason = strings.TrimSpace(string(r[:maxReasonRunes-1])) + "…"
	}
	return reason
}

// handleSkip acts on a skip intent from a sender already allowed to write.
func (s *Service) handleSkip(ctx context.Context, state PRState, in Intent, sender, op string) (Reaction, error) {
	ctx, cancel := writeContext(ctx)
	defer cancel()
	var err error
	switch in.Kind {
	case IntentSkipAsk:
		ask := SkipAsk{User: sender, Scope: in.Scope}
		if (state.PendingSkip != nil && *state.PendingSkip == ask) || (skipActive(state) && state.Skip.Scope == ask.Scope) {
			err = s.redrawSummary(ctx, state, op)
		} else {
			err = s.askSkipReason(ctx, state, ask, op)
		}
	case IntentSkip:
		err = s.skip(ctx, state, Skip{User: sender, Scope: in.Scope, Reason: cleanReason(in.Reason)}, op)
	case IntentSkipReason:
		if state.PendingSkip != nil {
			err = s.skip(ctx, state, Skip{User: state.PendingSkip.User, Scope: state.PendingSkip.Scope, Reason: cleanReason(in.Reason)}, op)
		}
	case IntentNone, IntentApply, IntentApplyAll, IntentRerun:
	}
	if err != nil {
		return "", err
	}
	return ReactionDone, nil
}

func (s *Service) askSkipReason(ctx context.Context, state PRState, ask SkipAsk, op string) error {
	body := fmt.Sprintf("@%s, post your reason for skipping this %s as a new comment; your next comment on this PR becomes the reason. Or skip in one step: `%s <reason>`.", ask.User, ask.Scope.noun(), ask.Scope.command())
	if _, err := s.gh.CreateIssueComment(ctx, state.InstallationID, state.Owner, state.Repo, state.Number, body); err != nil {
		return fmt.Errorf("%s: ask for skip reason: %w", op, err)
	}
	state.PendingSkip = &ask
	return s.saveAndRedraw(ctx, state, History{}, op)
}

// skip concludes the check run as skipped. A skip already in state only redraws
// the summary, which finishes a run that failed after saving.
func (s *Service) skip(ctx context.Context, state PRState, sk Skip, op string) error {
	if state.Skip != nil {
		stored := *state.Skip
		// Older versions stored reasons with zero-width spaces; cleanReason drops them.
		stored.Reason = cleanReason(stored.Reason)
		if stored == (Skip{User: sk.User, Scope: sk.Scope, Reason: sk.Reason, HeadSHA: state.HeadSHA}) {
			return s.redrawSummary(ctx, state, op)
		}
	}
	state, run, history := OnSkip(state, sk, time.Now())
	if state.CheckRunID == 0 {
		id, err := s.gh.CreateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, run)
		if err != nil {
			return fmt.Errorf("%s: create check run: %w", op, err)
		}
		state.CheckRunID = id
	} else if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("%s: update check run %d: %w", op, state.CheckRunID, err)
	}
	return s.saveAndRedraw(ctx, state, history, op)
}

// saveAndRedraw saves state and history before redrawing the summary, so a failed redraw
// never leaves a concluded check run with unsaved state; a retry redraws again.
func (s *Service) saveAndRedraw(ctx context.Context, state PRState, history History, op string) error {
	if err := s.store.SavePR(ctx, state, history); err != nil {
		return fmt.Errorf("%s: save state: %w", op, err)
	}
	return s.redrawSummary(ctx, state, op)
}
