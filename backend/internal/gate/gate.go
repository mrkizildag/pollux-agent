// Package gate is the domain: deciding what check run a pull request gets.
package gate

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// setupGuideURL is linked from the neutral check when no runner is available.
const setupGuideURL = "https://github.com/mrkizildag/pollux-agent/blob/main/docs/guides/setup.md"

// PullRequest is the subset of a GitHub pull request the gate needs.
type PullRequest struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	BaseSHA        string
	HeadSHA        string
	HeadRef        string // branch name of the head
	Fork           bool   // head lives in another repository, so Apply cannot push to it
	Open           bool   // false once the pull request is closed or merged
}

// Conclusion is a GitHub check run conclusion.
type Conclusion string

const (
	ConclusionSuccess        Conclusion = "success"
	ConclusionNeutral        Conclusion = "neutral"
	ConclusionActionRequired Conclusion = "action_required"
)

// Status is a GitHub check run status.
type Status string

const (
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
)

// CheckRun is a GitHub check run; Conclusion is empty unless Status is completed.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     Status
	Conclusion Conclusion
	Title      string
	Summary    string
}

// GitHub creates check runs and inspects repository state on behalf of an
// installation.
type GitHub interface {
	// CreateCheckRun returns the ID of the check run it created.
	CreateCheckRun(ctx context.Context, installationID int64, owner, repo string, run CheckRun) (int64, error)
	// GetPullRequest returns the pull request's current base and head commits.
	GetPullRequest(ctx context.Context, installationID int64, owner, repo string, number int) (PullRequest, error)
	UpdateCheckRun(ctx context.Context, installationID int64, owner, repo string, id int64, run CheckRun) error
	// WorkflowExists reports whether the repo's default branch has the
	// pollux-agent Actions workflow.
	WorkflowExists(ctx context.Context, installationID int64, owner, repo string) (bool, error)
	// MergeBase returns the merge base commit of base and head, the commit the pull request's diff starts from.
	MergeBase(ctx context.Context, installationID int64, owner, repo, base, head string) (string, error)
	// DocsExist reports whether an entry named docs exists at ref, be it a
	// directory, a file or a submodule.
	DocsExist(ctx context.Context, installationID int64, owner, repo, ref string) (bool, error)
	// ListChangedFiles returns the files in the pull request's diff with their head-side hunk ranges.
	ListChangedFiles(ctx context.Context, installationID int64, owner, repo string, number int) ([]review.ChangedFile, error)
	// ListComments returns the pull request's review comments and issue comments.
	ListComments(ctx context.Context, installationID int64, owner, repo string, number int) ([]Comment, error)
	CreateReviewComment(ctx context.Context, installationID int64, owner, repo string, number int, c ReviewComment) (Comment, error)
	EditReviewComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
	// ResolveReviewThread resolves the review thread that starts with comment
	// commentID. A thread that is already resolved or gone is not an error.
	ResolveReviewThread(ctx context.Context, installationID int64, owner, repo string, number int, commentID int64) error
	CreateIssueComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (Comment, error)
	EditIssueComment(ctx context.Context, installationID int64, owner, repo string, id int64, body string) error
}

// CommentKind says which GitHub comment API a Comment lives in; the two have
// separate ID spaces and edit endpoints.
type CommentKind string

const (
	CommentKindReview CommentKind = "review"
	CommentKindIssue  CommentKind = "issue"
)

// Comment is a comment on a pull request. Path, StartLine and Line locate a
// review comment and are zero for issue comments and for review comments GitHub
// no longer anchors. Mine is true when the App's bot user wrote the comment;
// only those are ever adopted or edited.
type Comment struct {
	ID        int64
	Mine      bool
	Kind      CommentKind
	URL       string
	Body      string
	Path      string
	StartLine int
	Line      int
}

// ReviewComment is a new review comment on the right side of a file in the
// head commit. StartLine 0 means a single-line comment on Line. File means a
// file-level comment on Path, with no line.
type ReviewComment struct {
	CommitSHA string
	Path      string
	StartLine int
	Line      int
	File      bool
	Body      string
}

// CheckName is the name of the check run pollux reports on every PR.
const CheckName = "pollux-agent"

// WorkflowPath is the target-repo workflow whose presence selects the Actions
// runner and whose completion carries its result.
const WorkflowPath = ".github/workflows/pollux-agent.yml"

// PRState is what the gate remembers about one pull request between events.
type PRState struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	HeadSHA        string // head commit the gate last reported a check run for; "" if never
	CheckRunID     int64  // check run reported for HeadSHA; 0 if none
	HeadRef        string // branch Apply commits to
	ProposalsSHA   string // head the proposals were last computed at; "" if never
	Fork           bool   // head lives in another repository; Apply is not offered
	Run            *AwaitingRun

	PendingSkip  *SkipAsk      // skip waiting for its reason; nil if none
	Skip         *Skip         // active skip; nil if none
	PendingApply *PendingApply // Apply commit being created; nil if none

	SummaryCommentID int64                    // 0 until the summary comment is created
	FailureCause     string                   // why the last analysis failed, shown in the summary; "" when it did not
	Dropped          []review.DroppedProposal // notice from the last completed analysis
	Proposals        []ProposalState
}

// History is the records a state transition emits; the caller passes them to
// SavePR so the Store writes them in the same transaction as the state.
type History struct {
	Analyses []Analysis // the store keeps one row per run nonce, the last saved
	Events   []PREvent
}

// RunnerKind is which analysis runner ran.
type RunnerKind string

const (
	RunnerKindActions RunnerKind = "actions"
	RunnerKindServer  RunnerKind = "server"
)

// AnalysisVerdict is how a recorded analysis ended.
type AnalysisVerdict string

const (
	VerdictNoImpact   AnalysisVerdict = "no_impact"
	VerdictProposals  AnalysisVerdict = "proposals"
	VerdictFailed     AnalysisVerdict = "failed"
	VerdictSuperseded AnalysisVerdict = "superseded"
)

// Analysis is the record of one analysis run, unique per Nonce of a pull
// request; a later save of the same Nonce replaces it. Reason is the one-line
// no-impact reason or the failure cause.
type Analysis struct {
	Nonce      string
	HeadSHA    string
	Runner     RunnerKind
	Model      string
	Verdict    AnalysisVerdict
	Reason     string
	Proposals  int
	StartedAt  time.Time
	FinishedAt time.Time
	RunID      int64
	Usage      *review.Usage // nil when the runner reported none
}

// EventKind is what happened to a proposal.
type EventKind string

const (
	EventApplied  EventKind = "applied"
	EventSkipped  EventKind = "skipped"
	EventOutdated EventKind = "outdated"
)

// PREvent is the record of one user action or outdating on a proposal. Key is
// unique per pull request, so saving the same event twice writes it once.
// ProposalID is empty for a skip made while no proposal was open.
type PREvent struct {
	Key        string
	Kind       EventKind
	Actor      string
	ProposalID string
	Scope      SkipScope
	Reason     string
	CommitSHA  string
	HeadSHA    string
}

// analysisBase is the record of run, awaited at head, ended at now with
// verdict; the caller fills Reason, Proposals, Model and Usage.
func analysisBase(run *AwaitingRun, head string, verdict AnalysisVerdict, now time.Time) *Analysis {
	return &Analysis{
		Nonce: run.Nonce, HeadSHA: head, Runner: run.Runner, Verdict: verdict,
		StartedAt: run.StartedAt, FinishedAt: now, RunID: run.RunID,
	}
}

func appliedEvent(id, sha, by, head string) PREvent {
	return PREvent{Key: fmt.Sprintf("applied/%s/%s", id, sha), Kind: EventApplied, Actor: by, ProposalID: id, CommitSHA: sha, HeadSHA: head}
}

func outdatedEvent(id, head string) PREvent {
	return PREvent{Key: fmt.Sprintf("outdated/%s/%s", id, head), Kind: EventOutdated, ProposalID: id, HeadSHA: head}
}

func skippedEvent(skip Skip, id string) PREvent {
	return PREvent{
		Key:  fmt.Sprintf("skipped/%s/%s/%s/%s", skip.Scope, skip.HeadSHA, skip.User, id),
		Kind: EventSkipped, Actor: skip.User, ProposalID: id, Scope: skip.Scope, Reason: skip.Reason, CommitSHA: skip.HeadSHA, HeadSHA: skip.HeadSHA,
	}
}

// PendingApply is an Apply commit that may exist on GitHub before its proposals
// are saved as applied: the proposals, the message and the parent it was built on.
type PendingApply struct {
	IDs     []string
	Message string
	Parent  string
	By      string // who asked for the apply, credited when a crash leaves it to adoption
}

// SkipScope is how long a skip passes the check.
type SkipScope string

const (
	SkipCommit SkipScope = "commit" // the head the skip was made at only
	SkipPR     SkipScope = "pr"     // every later push to the pull request
)

// SkipAsk is a skip the bot asked User for a reason for; User's next comment
// on the pull request becomes the reason.
type SkipAsk struct {
	User  string
	Scope SkipScope
}

// Skip waives the docs check. HeadSHA is the head it was made at.
type Skip struct {
	User    string
	Scope   SkipScope
	Reason  string
	HeadSHA string
}

// AwaitingRun is the external analysis run whose result will conclude the
// check run; PRState.Run is nil when none is awaited.
type AwaitingRun struct {
	RunID    int64
	Nonce    string
	Deadline time.Time
	BaseSHA  string // merge base the run was started at
	// StartedAt and Runner are the only record of a run that ends in failure.
	StartedAt time.Time
	Runner    RunnerKind
}

// RunCompleted reports that an external analysis run finished.
type RunCompleted struct {
	InstallationID int64
	Owner          string
	Repo           string
	Number         int
	RunID          int64
	Conclusion     string
}

// pullRequest is the pull request state describes, at the head it last reported.
func (s PRState) pullRequest() PullRequest {
	return PullRequest{InstallationID: s.InstallationID, Owner: s.Owner, Repo: s.Repo, Number: s.Number, HeadSHA: s.HeadSHA, HeadRef: s.HeadRef, Fork: s.Fork}
}

// PRRef identifies a pull request.
type PRRef struct {
	Owner  string
	Repo   string
	Number int
}

// RerunRequest asks for a fresh analysis of a pull request's current head.
// SummaryCommentID is the comment whose Re-run box was ticked; 0 means any
// request is accepted.
type RerunRequest struct {
	InstallationID   int64
	PRRef            PRRef
	SummaryCommentID int64
}

// OverdueRun is an awaited run whose deadline has passed, as found by a sweep.
type OverdueRun struct {
	PRRef         // Number is 0 for a scaffold
	Scaffold bool // the run writes the repo's scaffold
	Nonce    string
	Deadline time.Time
}

// ProposalStatus is whether a proposal still applies to the latest head.
type ProposalStatus string

const (
	ProposalOpen     ProposalStatus = "open"
	ProposalOutdated ProposalStatus = "outdated"
	ProposalApplied  ProposalStatus = "applied"
)

// ProposalState is one proposal's review comment as the gate remembers it.
type ProposalState struct {
	ID         string // see ProposalID
	DocPath    string
	Section    string
	CommentID  int64 // 0 until the review comment is created
	CommentURL string
	State      ProposalStatus

	Content    string // the section as proposed, heading included
	Original   string // the section text Content replaces; "" for a new doc
	IndexEntry string
	AppliedSHA string // commit that applied it; "" unless Applied
	ReplyID    int64  // reply posted under the comment for AppliedSHA; 0 if none
}

// ProposalID is the stable identity of a proposal across re-runs: a short hash
// of its doc path and normalized section heading (path alone for a new doc).
func ProposalID(docPath, section string) string {
	sum := sha256.Sum256([]byte(docPath + "\x00" + headingText(section)))
	return hex.EncodeToString(sum[:6])
}

// headingText is a heading line without its leading '#' marks and surrounding
// whitespace, so differently marked spellings of one heading compare equal.
func headingText(s string) string {
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(s), "#"))
}

// Store persists PRState.
type Store interface {
	// LoadPR returns the zero-HeadSHA state (identity fields filled from the args) for a PR never saved.
	LoadPR(ctx context.Context, owner, repo string, number int) (PRState, error)
	// SavePR saves state and history in one transaction. Saving the same
	// history again is harmless: an analysis is upserted per nonce and an event
	// with a known key is ignored.
	SavePR(ctx context.Context, state PRState, history History) error
	// PRForRun returns the pull request an external run was dispatched for.
	PRForRun(ctx context.Context, owner, repo string, runID int64) (number int, ok bool, err error)
	// LoadScaffold returns the Idle zero-Attempt state (identity fields filled from the args) for a repo never saved.
	LoadScaffold(ctx context.Context, owner, repo string) (ScaffoldState, error)
	SaveScaffold(ctx context.Context, state ScaffoldState) error
	// ScaffoldForRun reports whether the repo's scaffold awaits the external run runID.
	ScaffoldForRun(ctx context.Context, owner, repo string, runID int64) (bool, error)
	// RequestScaffold atomically creates the repo's Idle scaffold state if it has
	// none and records waiter, returning the state as it stands. Existing state is
	// untouched except that its installation ID becomes installationID.
	RequestScaffold(ctx context.Context, installationID int64, owner, repo string, waiter ScaffoldWaiter) (ScaffoldState, error)
	// UnlinkedScaffoldWaiters returns the repo's recorded check runs not yet marked linked, oldest first.
	UnlinkedScaffoldWaiters(ctx context.Context, owner, repo string) ([]ScaffoldWaiter, error)
	// MarkScaffoldWaiterLinked records that the check run no longer waits for the scaffold.
	MarkScaffoldWaiterLinked(ctx context.Context, owner, repo string, checkRunID int64) error
}

// OnPush is the state transition for a new head commit: pure, no I/O. It
// drops any awaited run, so that run's result is ignored; the proposals, the
// summary comment and PR-scope skips carry over. A commit-scope skip carries
// over only for the head it was made at, and a pending skip ask, of either
// scope, only while the head stays the same. The pending apply is dropped.
// A run still awaited is dropped and recorded as superseded at now in the
// returned History.
func OnPush(prev PRState, pr PullRequest, now time.Time) (PRState, History) {
	next := PRState{
		InstallationID:   pr.InstallationID,
		Owner:            pr.Owner,
		Repo:             pr.Repo,
		Number:           pr.Number,
		HeadSHA:          pr.HeadSHA,
		HeadRef:          pr.HeadRef,
		Fork:             pr.Fork,
		ProposalsSHA:     prev.ProposalsSHA,
		SummaryCommentID: prev.SummaryCommentID,
		Proposals:        slices.Clone(prev.Proposals),
		Dropped:          slices.Clone(prev.Dropped),
	}
	if prev.Skip != nil {
		kept := *prev.Skip
		next.Skip = &kept
		if !skipActive(next) {
			next.Skip = nil
		}
	}
	if ask := prev.PendingSkip; ask != nil && pendingSkipCancelled(prev, pr) == nil {
		kept := *ask
		next.PendingSkip = &kept
	}
	var history History
	if prev.Run != nil {
		history.Analyses = []Analysis{*analysisBase(prev.Run, prev.HeadSHA, VerdictSuperseded, now)}
	}
	return next, history
}

// pendingSkipCancelled returns the pending skip ask that a push to pr cancels,
// or nil: any ask is cancelled by a new head, kept on the same one.
func pendingSkipCancelled(prev PRState, pr PullRequest) *SkipAsk {
	if prev.PendingSkip == nil || prev.HeadSHA == pr.HeadSHA {
		return nil
	}
	return prev.PendingSkip
}

// Superseded returns the neutral check run that closes the check run of an
// awaited run when a new analysis of pr replaces it, on any head; ok is false
// when there is nothing to close. Pure, no I/O.
func Superseded(state PRState, pr PullRequest) (run CheckRun, ok bool) {
	if state.Run == nil || state.CheckRunID == 0 {
		return CheckRun{}, false
	}
	by := "a re-run"
	if pr.HeadSHA != state.HeadSHA {
		by = shortSHA(pr.HeadSHA)
	}
	run = CheckRun{Name: CheckName, HeadSHA: state.HeadSHA, Status: StatusCompleted}
	return neutral(run, "Superseded", "Superseded by "+by), true
}

// Overdue reports whether state awaits a run whose deadline has passed at now.
func Overdue(state PRState, now time.Time) bool {
	return state.Run != nil && now.After(state.Run.Deadline)
}

// OnStarted is the state transition for an analysis that runs elsewhere: pure, no I/O.
func OnStarted(state PRState, pending review.Pending, checkRunID int64, baseSHA string) PRState {
	state.CheckRunID = checkRunID
	var run AwaitingRun
	if state.Run != nil {
		run = *state.Run
	}
	run.RunID, run.Nonce, run.Deadline, run.BaseSHA = pending.RunID, pending.Nonce, pending.Deadline, baseSHA
	state.Run = &run
	return state
}

// MatchesRun reports whether rc is the completion of the run state awaits.
func MatchesRun(state PRState, rc RunCompleted) bool {
	return state.Run != nil && rc.RunID != 0 && state.Run.RunID == rc.RunID
}

// Outcome is how an analysis ended: exactly one of Result or Failed is set.
type Outcome struct {
	Result *review.Result
	Failed *AnalysisFailed
}

func resultOutcome(r review.Result) Outcome { return Outcome{Result: &r} }

func failedOutcome(cause string) Outcome { return Outcome{Failed: &AnalysisFailed{Cause: cause}} }

// AnalysisFailed is an analysis that ended without a usable Result. Title is
// the check run title; empty means "Analysis failed".
type AnalysisFailed struct {
	Title string
	Cause string
}

const titleTooLarge = "PR too large to analyze"

// tooLargeError is a pull request over the size limits; limit says which.
type tooLargeError struct{ limit string }

func (e *tooLargeError) Error() string { return titleTooLarge + ": " + e.limit }

const (
	maxChangedFiles = 50
	maxPatchBytes   = 1 << 20
)

// oversized reports whether changed is too large to analyze, and the limit hit.
// A file with changes but no patch text counts as over the patch limit: GitHub
// omits the patch of a diff too large to return. Binary files have no changes.
func oversized(changed []review.ChangedFile) (limit string, ok bool) {
	if len(changed) > maxChangedFiles {
		return fmt.Sprintf("%d changed files; the limit is %d.", len(changed), maxChangedFiles), true
	}
	total := 0
	for _, f := range changed {
		if f.Patch == "" && f.Changes > 0 {
			return fmt.Sprintf("GitHub omitted the diff of a changed file; the limit is %d bytes of patch text.", maxPatchBytes), true
		}
		total += len(f.Patch)
	}
	if total > maxPatchBytes {
		return fmt.Sprintf("%d bytes of patch text; the limit is %d bytes.", total, maxPatchBytes), true
	}
	return "", false
}

const (
	// maxSummaryBytes is GitHub's limit on a check run summary.
	maxSummaryBytes = 65535
	maxCauseBytes   = 1000
	truncatedMark   = "… (truncated)"
	// writeTimeout bounds the state writes that must survive a cancelled job.
	writeTimeout = 30 * time.Second
	// collectAttempts is how many times a result download is tried.
	collectAttempts = 3
	// postAttempts is how many times posting a result's comments is tried.
	postAttempts = 3
)

// AnalysisDeadline bounds the armed placeholder before the deadline sweep
// concludes its check run, and is also the deadline of the Actions review run.
const AnalysisDeadline = 5 * time.Minute

// truncate cuts s to at most max bytes on a UTF-8 boundary, ending in a marker
// when it cut anything.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max - len(truncatedMark)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + truncatedMark
}

// shortSHA is the 7-character abbreviation of sha.
func shortSHA(sha string) string { return sha[:min(7, len(sha))] }

// conclude is the state transition for an analysis that ended: pure, no I/O.
// It clears the awaited run and returns the completed check run to report and
// the analysis record, when a run was awaited; runner-supplied text is capped
// to what GitHub accepts. An active skip replaces the outcome with its success.
func conclude(state PRState, outcome Outcome, now time.Time) (PRState, CheckRun, History) {
	state.FailureCause = ""
	state.Dropped = nil
	if outcome.Result != nil {
		state.Dropped = boundedDropped(outcome.Result.Dropped)
	}
	if outcome.Failed != nil {
		state.FailureCause = truncate(outcome.Failed.Cause, maxCauseBytes)
	}
	var history History
	if state.Run != nil {
		history.Analyses = []Analysis{*analysisOf(state.Run, state.HeadSHA, outcome, now)}
	}
	if skipActive(state) {
		state.Run = nil
		return state, skipRun(state), history
	}
	state, run := concludeUncapped(state, outcome)
	run = withDroppedNotice(run, state.Dropped)
	return state, run, history
}

// boundedDropped owns the persisted notice bounds and counts each rejected index once.
func boundedDropped(raw []review.DroppedProposal) []review.DroppedProposal {
	var out []review.DroppedProposal
	seen := map[int]bool{}
	for _, d := range raw {
		if d.Index < 0 || seen[d.Index] {
			continue
		}
		seen[d.Index] = true
		d.Reason = truncate(oneLine(d.Reason), 512)
		out = append(out, d)
		if len(out) == 20 {
			break
		}
	}
	return out
}

// withDroppedNotice reserves room for the whole bounded notice in check output.
func withDroppedNotice(run CheckRun, dropped []review.DroppedProposal) CheckRun {
	notice := droppedNotice(dropped)
	if notice == "" {
		run.Summary = truncate(run.Summary, maxSummaryBytes)
		return run
	}
	run.Summary = truncate(run.Summary, maxSummaryBytes-len(notice)-2) + "\n\n" + notice
	return run
}

// analysisOf is the record of run, awaited at head, ended as outcome says at
// now. Its verdict is the one the check run reports, even under an active skip.
func analysisOf(run *AwaitingRun, head string, outcome Outcome, now time.Time) *Analysis {
	verdict, reason, proposals := classify(outcome)
	a := analysisBase(run, head, verdict, now)
	a.Reason, a.Proposals = truncate(reason, maxCauseBytes), len(proposals)
	if outcome.Result != nil {
		a.Model, a.Usage = outcome.Result.Model, outcome.Result.Usage
	}
	return a
}

// classify is how outcome ends an analysis: the verdict recorded for it and
// that the check run reports, its one-line reason, and its proposals.
func classify(outcome Outcome) (verdict AnalysisVerdict, reason string, proposals review.Proposals) {
	switch {
	case outcome.Result != nil:
		switch v := outcome.Result.Verdict.(type) {
		case review.NoImpact:
			return VerdictNoImpact, v.Reason, nil
		case review.Proposals:
			if len(v) == 0 {
				return VerdictFailed, "runner returned an empty proposal list; no impact must be NoImpact", nil
			}
			return VerdictProposals, "", v
		default:
			return VerdictFailed, fmt.Sprintf("unknown review.Verdict %T", v), nil
		}
	case outcome.Failed != nil:
		return VerdictFailed, truncate(outcome.Failed.Cause, maxCauseBytes), nil
	default:
		return VerdictFailed, "analysis ended without an outcome", nil
	}
}

func concludeUncapped(state PRState, outcome Outcome) (PRState, CheckRun) {
	state.Run = nil
	run := CheckRun{Name: CheckName, HeadSHA: state.HeadSHA, Status: StatusCompleted}

	verdict, reason, proposals := classify(outcome)
	switch verdict {
	case VerdictNoImpact:
		run.Conclusion, run.Title, run.Summary = ConclusionSuccess, "No doc impact", inertProse(reason)
		return state, run
	case VerdictProposals:
		pending := unapplied(state, proposals)
		if len(pending) == 0 {
			run.Conclusion, run.Title, run.Summary = ConclusionSuccess, "Docs up to date", "Every proposed doc change is already applied."
			return state, run
		}
		run.Conclusion, run.Title, run.Summary = ConclusionActionRequired, "Docs need updating", proposalsSummary(pending)
		return state, run
	case VerdictFailed, VerdictSuperseded:
	}
	title := "Analysis failed"
	if outcome.Failed != nil {
		title = cmp.Or(outcome.Failed.Title, title)
	}
	return state, neutral(run, title, reason)
}

// unapplied is v without the proposals state already holds as applied with the
// same content; Reconcile neither reopens nor reposts those.
func unapplied(state PRState, v review.Proposals) review.Proposals {
	var out review.Proposals
	for _, p := range v {
		id := ProposalID(p.DocPath, p.Section)
		if !slices.ContainsFunc(state.Proposals, func(ps ProposalState) bool { return ps.ID == id && appliedAs(ps, p) }) {
			out = append(out, p)
		}
	}
	return out
}

func appliedAs(ps ProposalState, p review.Proposal) bool {
	return ps.State == ProposalApplied && sameContent(p.Section, ps.Content, withHeading(p).Content)
}

// sameContent reports whether two contents of a proposal differ at most in
// insignificant whitespace at the end and, for a section, in how its heading
// line is spelled.
func sameContent(section, a, b string) bool {
	a, b = strings.TrimRightFunc(a, unicode.IsSpace), strings.TrimRightFunc(b, unicode.IsSpace)
	if section == "" {
		return a == b
	}
	headA, restA, _ := strings.Cut(a, "\n")
	headB, restB, _ := strings.Cut(b, "\n")
	return restA == restB && headingText(headA) == headingText(headB)
}

func neutral(run CheckRun, title, summary string) CheckRun {
	run.Conclusion, run.Title, run.Summary = ConclusionNeutral, title, summary
	return run
}

// CommentWrite is a comment write Reconcile asks the Service to perform.
// Summary writes target the summary comment; the Service renders its body from
// the final state because it links comments created by earlier writes.
// Otherwise Index is the proposal in State.Proposals. ID 0 means create (Review
// holds the new review comment); else edit comment ID to Body, and when Resolve
// is set also resolve its review thread.
type CommentWrite struct {
	Summary bool
	Index   int
	ID      int64
	Review  ReviewComment
	Body    string
	Resolve bool
}

// Reconcile is the state transition for a finished run: pure, no I/O. It
// returns prev with only Proposals, ProposalsSHA and SummaryCommentID changed,
// the comment writes that realize it, ordered retire, then edits and creates,
// then the summary, and an outdated event per proposal a push made stale, to be
// saved with the state. Created comments' IDs and URLs belong in the
// returned state at the writes' Index. The rules are in
// docs/features/proposal-output.md.
func Reconcile(prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile, existing []Comment) (PRState, []CommentWrite, History) {
	next := prev
	next.Proposals = slices.Clone(prev.Proposals)
	var history History
	next.ProposalsSHA = pr.HeadSHA
	proposals, _ := verdict.(review.Proposals)

	index := make(map[string]int, len(next.Proposals))
	for i, ps := range next.Proposals {
		index[ps.ID] = i
	}
	current := make(map[string]bool, len(proposals))
	var retires, writes []CommentWrite

	for _, p := range proposals {
		id := ProposalID(p.DocPath, p.Section)
		if current[id] {
			continue
		}
		current[id] = true
		i, ok := index[id]
		if !ok {
			i = len(next.Proposals)
			index[id] = i
			next.Proposals = append(next.Proposals, ProposalState{ID: id})
		}
		ps := &next.Proposals[i]
		if appliedAs(*ps, p) {
			continue
		}
		p = withHeading(p)
		prior := ps.State
		contentChanged := !sameContent(p.Section, ps.Content, p.Content)
		ps.DocPath, ps.Section, ps.State = p.DocPath, p.Section, ProposalOpen
		ps.Content, ps.Original, ps.IndexEntry = p.Content, p.Original, p.IndexEntry
		ps.AppliedSHA, ps.ReplyID = "", 0
		if prior == "" || prior == ProposalOpen {
			adoptMarked(ps, existing)
		}
		rc := proposalComment(pr.HeadSHA, id, p, changed, pr.Fork)
		retire := prior == ProposalApplied || prior == ProposalOutdated || (prior == ProposalOpen && contentChanged)
		old, found := findComment(existing, CommentKindReview, ps.CommentID)
		switch {
		case retire:
			if found {
				retires = append(retires, retireWrite(i, id, old))
			}
		case found:
			body := rc.Body
			if !sameAnchor(old, rc) {
				body = renderCheckbox(id, p, pr.Fork)
			}
			writes = append(writes, CommentWrite{Index: i, ID: old.ID, Body: body})
			continue
		}
		ps.CommentID, ps.CommentURL = 0, ""
		writes = append(writes, CommentWrite{Index: i, Review: rc})
	}

	for i := range next.Proposals {
		ps := &next.Proposals[i]
		if current[ps.ID] || ps.State == ProposalOutdated || ps.State == ProposalApplied {
			continue
		}
		adoptMarked(ps, existing)
		ps.State = ProposalOutdated
		history.Events = append(history.Events, outdatedEvent(ps.ID, pr.HeadSHA))
		if c, ok := findComment(existing, CommentKindReview, ps.CommentID); ok {
			retires = append(retires, retireWrite(i, ps.ID, c))
		}
	}
	writes = slices.Concat(retires, writes)

	summaryLost := resolveSummary(&next, existing)
	if next.SummaryCommentID != 0 || len(proposals) > 0 || summaryLost {
		writes = append(writes, CommentWrite{Summary: true, ID: next.SummaryCommentID})
	}
	return next, writes, history
}

// retireWrite swaps comment c's live marker for the superseded one and
// resolves its thread; repeating it is harmless.
func retireWrite(i int, id string, c Comment) CommentWrite {
	return CommentWrite{Index: i, ID: c.ID, Body: renderSuperseded(id, c.Body), Resolve: true}
}

// ReconcileFailure is the state transition for a failed run: pure, no I/O. It
// returns prev with only SummaryCommentID possibly changed, and the one summary
// write that reports the failure. Proposals are left as they are, so earlier
// ones stay listed.
func ReconcileFailure(prev PRState, existing []Comment) (PRState, CommentWrite) {
	next := prev
	resolveSummary(&next, existing)
	return next, CommentWrite{Summary: true, ID: next.SummaryCommentID}
}

// resolveSummary points next at our existing summary comment when state lacks
// a live ID for it, and reports whether state's summary comment has vanished
// with no marked one to adopt.
func resolveSummary(next *PRState, existing []Comment) (lost bool) {
	if _, ok := findComment(existing, CommentKindIssue, next.SummaryCommentID); ok {
		return false
	}
	lost = next.SummaryCommentID != 0
	next.SummaryCommentID = 0
	if c, ok := findMarked(existing, CommentKindIssue, summaryMarker); ok {
		next.SummaryCommentID, lost = c.ID, false
	}
	return lost
}

// withHeading restores the section's heading (and the blank lines after it)
// when a runner's Content omits it, so rendering and applying it never drop
// the heading from the doc.
func withHeading(p review.Proposal) review.Proposal {
	if p.Section == "" || p.Original == "" {
		return p
	}
	heading, rest, _ := strings.Cut(p.Original, "\n")
	if first, _, _ := strings.Cut(strings.TrimLeft(p.Content, " \t\r\n"), "\n"); headingLevel(first) == headingLevel(heading) {
		return p
	}
	lead := heading + "\n"
	for strings.HasPrefix(rest, "\n") {
		lead += "\n"
		rest = rest[1:]
	}
	p.Content = lead + strings.TrimLeft(p.Content, "\n")
	return p
}

// headingLevel is the ATX level of line ("## x" is 2), or 0 when it is not a heading.
func headingLevel(line string) int {
	line = strings.TrimSpace(line)
	level := len(line) - len(strings.TrimLeft(line, "#"))
	if level == 0 || level > 6 || (len(line) > level && line[level] != ' ') {
		return 0
	}
	return level
}

// adoptMarked records our existing review comment carrying ps's marker when
// state has no comment ID for it (a run stopped before saving).
func adoptMarked(ps *ProposalState, existing []Comment) {
	if ps.CommentID != 0 {
		return
	}
	if c, ok := findMarked(existing, CommentKindReview, proposalMarker(ps.ID)); ok {
		ps.CommentID, ps.CommentURL = c.ID, c.URL
	}
}

func findMarked(existing []Comment, kind CommentKind, marker string) (Comment, bool) {
	for _, c := range existing {
		if c.Mine && c.Kind == kind && hasMarker(c.Body, marker) {
			return c, true
		}
	}
	return Comment{}, false
}

// hasMarker reports whether marker is the first line of body.
func hasMarker(body, marker string) bool {
	first, _, _ := strings.Cut(body, "\n")
	return strings.TrimRight(first, "\r") == marker
}

// findComment returns our comment id; a listed comment someone else wrote
// counts as missing so it is never edited.
func findComment(existing []Comment, kind CommentKind, id int64) (Comment, bool) {
	for _, c := range existing {
		if c.Mine && c.Kind == kind && c.ID == id {
			return c, true
		}
	}
	return Comment{}, false
}

// sameAnchor reports whether existing sits where rc would be created; a
// suggestion body is only safe to write onto the lines it was computed for. A
// file-level comment has Line 0 on both sides.
func sameAnchor(existing Comment, rc ReviewComment) bool {
	return existing.Path == rc.Path && existing.StartLine == rc.StartLine && existing.Line == rc.Line
}

// Runners are the analysis runners a repo may use. A nil Runner means that
// runner is unavailable.
type Runners struct {
	Actions ActionsRunner
	Server  ServerRunner
}

// ActionsRunner is the runner that works in the repo's Actions workflow: it
// reviews PRs and writes scaffolds, both completing through a webhook.
type ActionsRunner interface {
	review.AsyncRunner
	review.AsyncScaffolder
}

// ServerRunner is the runner that works on the server: it reviews PRs and
// writes scaffolds in one call.
type ServerRunner interface {
	review.Runner
	review.Scaffolder
}

// Service decides and reports the pollux-agent check run for a pull request.
type Service struct {
	gh            GitHub
	store         Store
	runners       Runners
	comments      CommentGitHub
	scaffoldGH    ScaffoldGitHub
	scaffoldQueue ScaffoldQueue
	log           *slog.Logger
	// collectBackoff is the wait before the first Collect retry; it doubles.
	collectBackoff time.Duration
}

// NewService returns a Service that reports check runs through gh, acts on
// comments through comments, persists state through store, selects among
// runners for analysis, and writes scaffolds through scaffoldGH, scheduling
// their jobs on scaffoldQueue.
func NewService(gh GitHub, comments CommentGitHub, store Store, runners Runners, scaffoldGH ScaffoldGitHub, scaffoldQueue ScaffoldQueue) *Service {
	return &Service{gh: gh, comments: comments, store: store, runners: runners, scaffoldGH: scaffoldGH, scaffoldQueue: scaffoldQueue, log: slog.New(slog.DiscardHandler), collectBackoff: time.Second}
}

// WithLogger sets where the Service logs failures it continues past, such as
// a review thread it could not resolve, and returns s.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	s.log = l
	return s
}

// WithCollectBackoff sets the wait before the first Collect retry (doubling
// each retry) and returns s.
func (s *Service) WithCollectBackoff(d time.Duration) *Service {
	s.collectBackoff = d
	return s
}

// HandlePullRequest selects an analysis runner for pr, runs it, and reports
// the result as the pollux-agent check run. A runner that finishes later leaves
// the check run in progress until HandleRunCompleted concludes it. A push of the
// commit a crashed Apply made keeps that Apply's proposals applied.
func (s *Service) HandlePullRequest(ctx context.Context, pr PullRequest) error {
	op := fmt.Sprintf("handle pull request %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	state, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("%s: load state: %w", op, err)
	}
	if err := s.analyzeHead(ctx, state, pr); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}

// analyzeHead analyzes pr's head against the stored state, as a push or a
// re-run does: the push of a crashed Apply's commit keeps its proposals
// applied, and a pending skip ask the new head cancels gets a note.
func (s *Service) analyzeHead(ctx context.Context, loaded PRState, pr PullRequest) error {
	op := fmt.Sprintf("adopt pending apply of %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	state, adopted, err := s.adoptPendingApply(ctx, loaded, pr, op)
	if err != nil {
		return err
	}
	// Once the adopted state is saved, a failed comment write must not stop the
	// analysis of the new head.
	var commentErr error
	if adopted {
		writeCtx, cancel := writeContext(ctx)
		_, commentErr = s.finishApply(writeCtx, state, loaded.PendingApply.IDs, loaded.PendingApply.IDs, "", op)
		cancel()
	}
	analyzeErr := errors.Join(commentErr, s.analyze(ctx, state, pr))
	noteCtx, cancel := writeContext(ctx)
	defer cancel()

	// The ask is cancelled once the new head is saved, so a retried job finds no
	// pending skip and the note is posted once; an analysis that failed before
	// saving leaves the ask for the retry.
	if ask := pendingSkipCancelled(loaded, pr); ask != nil && (analyzeErr == nil || s.headSaved(noteCtx, pr)) {
		label := skipCommitLabel
		if ask.Scope == SkipPR {
			label = skipPRLabel
		}
		body := fmt.Sprintf("@%s, a new push arrived before your reason, so the skip for `%s` was cancelled. Tick **%s** again to skip the new head.", ask.User, shortSHA(loaded.HeadSHA), label)
		if _, err := s.gh.CreateIssueComment(noteCtx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, body); err != nil {
			return errors.Join(analyzeErr, fmt.Errorf("post skip cancellation: %w", err))
		}
	}
	return analyzeErr
}

// headSaved reports whether the stored state is already at pr's head; a failed
// load counts as not saved.
func (s *Service) headSaved(ctx context.Context, pr PullRequest) bool {
	latest, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	return err == nil && latest.HeadSHA == pr.HeadSHA
}

// HandleRerun starts a fresh analysis of the pull request's current head, as a
// push would, unless r names a summary comment that is not the one state holds,
// the pull request is not open, or its current head is already being analyzed
// within its deadline.
func (s *Service) HandleRerun(ctx context.Context, r RerunRequest) error {
	ref := r.PRRef
	state, err := s.store.LoadPR(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("handle rerun of %s/%s#%d: load state: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if r.SummaryCommentID != 0 && r.SummaryCommentID != state.SummaryCommentID {
		return nil
	}
	pr, err := s.gh.GetPullRequest(ctx, r.InstallationID, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("handle rerun of %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if !pr.Open || (state.Run != nil && state.HeadSHA == pr.HeadSHA && !Overdue(state, time.Now())) {
		return nil
	}
	if err := s.analyzeHead(ctx, state, pr); err != nil {
		return fmt.Errorf("handle rerun of %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	return nil
}

// analyze closes the check run of any awaited analysis, then starts a new one
// on the runner the repo uses.
func (s *Service) analyze(ctx context.Context, state PRState, pr PullRequest) error {
	if old, ok := Superseded(state, pr); ok {
		if err := s.gh.UpdateCheckRun(ctx, state.InstallationID, state.Owner, state.Repo, state.CheckRunID, old); err != nil {
			return fmt.Errorf("supersede check run %d: %w", state.CheckRunID, err)
		}
	}

	if next, history := OnPush(state, pr, time.Now()); next.Skip != nil && next.Skip.Scope == SkipPR {
		return s.concludeSkipped(ctx, next, history, pr)
	}

	docsExist, err := s.gh.DocsExist(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.HeadSHA)
	if err != nil {
		return fmt.Errorf("look for docs/ at %s: %w", shortSHA(pr.HeadSHA), err)
	}

	var hasWorkflow bool
	if s.runners.Actions != nil || s.runners.Server != nil {
		hasWorkflow, err = s.gh.WorkflowExists(ctx, pr.InstallationID, pr.Owner, pr.Repo)
		if err != nil {
			return fmt.Errorf("find workflow: %w", err)
		}
	}
	selected := selectRunner(hasWorkflow, s.runners)

	if !docsExist {
		if selected == runnerNone {
			return s.concludeNoDocs(ctx, state, pr)
		}
		return s.requestScaffold(ctx, state, pr)
	}

	switch selected {
	case runnerActions:
		return s.startRun(ctx, state, pr, s.runners.Actions, RunnerKindActions)
	case runnerServer:
		return s.startRun(ctx, state, pr, s.runners.Server, RunnerKindServer)
	case runnerNone:
	}

	run := CheckRun{
		Name:       CheckName,
		HeadSHA:    pr.HeadSHA,
		Status:     StatusCompleted,
		Conclusion: ConclusionNeutral,
		Title:      "No analysis runner configured",
		Summary:    "Set up an analysis runner: " + setupGuideURL,
	}
	if _, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, run); err != nil {
		return fmt.Errorf("create check run: %w", err)
	}
	next, history := OnPush(state, pr, time.Now())
	if err := s.store.SavePR(ctx, next, history); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// concludeSkipped reports the active PR skip as the check run for the new head
// without starting any analysis.
func (s *Service) concludeSkipped(ctx context.Context, next PRState, history History, pr PullRequest) error {
	op := fmt.Sprintf("handle pull request %s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, skipRun(next))
	if err != nil {
		return fmt.Errorf("%s: create check run: %w", op, err)
	}
	next.CheckRunID = id
	if err := s.store.SavePR(ctx, next, history); err != nil {
		return fmt.Errorf("%s: save state: %w", op, err)
	}
	return nil
}

// startRun reports an in-progress check run, arms its deadline in saved state,
// then runs the analysis. The check run and the armed state come first so a
// failed or cancelled start can still close the check run, and the deadline
// sweep can if nothing else does; once started, the state writes outlive a
// cancelled ctx so the next job can find and close the check run.
func (s *Service) startRun(ctx context.Context, state PRState, pr PullRequest, runner review.Runner, kind RunnerKind) error {
	id, err := s.gh.CreateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, CheckRun{
		Name:    CheckName,
		HeadSHA: pr.HeadSHA,
		Status:  StatusInProgress,
		Title:   "Analyzing docs impact",
		Summary: "Waiting for the analysis to finish.",
	})
	if err != nil {
		return fmt.Errorf("create check run: %w", err)
	}

	now := time.Now()
	next, superseded := OnPush(state, pr, now)
	next.CheckRunID = id
	next.Run = &AwaitingRun{Nonce: fmt.Sprintf("check-%d", id), Deadline: now.Add(AnalysisDeadline), StartedAt: now, Runner: kind}

	var out startOutcome
	armCtx, cancelArm := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	err = s.store.SavePR(armCtx, next, superseded)
	cancelArm()
	var unsaved History
	if err != nil {
		unsaved = superseded
		err = fmt.Errorf("save state: %w", err)
	} else {
		out, err = s.start(ctx, runner, pr)
		if err != nil && ctx.Err() != nil {
			// Superseded or shutting down: the armed state stays so the next job
			// closes this check run as superseded, or the deadline sweep does.
			return fmt.Errorf("analysis of %s/%s#%d interrupted: %w", pr.Owner, pr.Repo, pr.Number, errors.Join(err, context.Cause(ctx)))
		}
	}

	// The write budget starts once the analysis returns; a server analysis can
	// take longer than writeTimeout on its own.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if err == nil {
		switch res := out.started.(type) {
		case review.Pending:
			next = OnStarted(next, res, id, out.mergeBase)
		case review.Result:
			if next, unsaved, err = s.concludeResult(writeCtx, next, pr, res, out.changed); err != nil {
				return err
			}
		default:
			err = fmt.Errorf("unknown review.Started %T", out.started)
		}
	}
	if err != nil {
		return s.failRun(writeCtx, next, pr, unsaved, err)
	}

	if err := s.store.SavePR(writeCtx, next, unsaved); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// failRun concludes the check run neutral for cause, reports it in the summary
// comment, and saves the concluded state with unsaved, the records no save has
// written yet. It returns cause, as a *reportedFailure when those steps
// succeeded, else joined with their error.
func (s *Service) failRun(ctx context.Context, state PRState, pr PullRequest, unsaved History, cause error) error {
	outcome := failedOutcome(failureCause(cause))
	var large *tooLargeError
	if errors.As(cause, &large) {
		outcome = Outcome{Failed: &AnalysisFailed{Title: titleTooLarge, Cause: large.limit}}
	}
	if err := s.concludeFailed(ctx, state, pr, unsaved, outcome); err != nil {
		return errors.Join(cause, err)
	}
	return &reportedFailure{cause}
}

// reportedFailure is an analysis failure that the check run and the summary
// already report, so a caller that only wants the analysis to have run is done.
type reportedFailure struct{ error }

func (r *reportedFailure) Unwrap() error { return r.error }

// concludeFailed concludes the check run neutral for a failed outcome, writes
// the summary comment with its cause, and saves the concluded state. If the
// check run cannot be concluded the armed state stays so the deadline sweep
// retries. unsaved is saved along with the concluded state's own records.
func (s *Service) concludeFailed(ctx context.Context, state PRState, pr PullRequest, unsaved History, outcome Outcome) error {
	next, run, history := conclude(state, outcome, time.Now())
	history.Analyses = append(slices.Clone(unsaved.Analyses), history.Analyses...)
	history.Events = append(slices.Clone(unsaved.Events), history.Events...)
	if err := s.gh.UpdateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, state.CheckRunID, run); err != nil {
		return fmt.Errorf("conclude check run %d: %w", state.CheckRunID, err)
	}
	var errs []error
	if withSummary, err := s.postFailureSummary(ctx, next, pr); err != nil {
		errs = append(errs, err)
	} else {
		next = withSummary
	}
	if err := s.store.SavePR(ctx, next, history); err != nil {
		errs = append(errs, fmt.Errorf("save state: %w", err))
	}
	return errors.Join(errs...)
}

// concludeResult concludes the check run for a result, then, when the verdict
// concludes the analysis, saves state and posts its comments, retrying the posts
// with backoff. If every post fails the run is re-armed so the deadline sweep
// ends the check neutral. Callers save the returned state with the returned
// history, the records no save has written yet.
func (s *Service) concludeResult(ctx context.Context, state PRState, pr PullRequest, res review.Result, changed []review.ChangedFile) (PRState, History, error) {
	next, run, history := conclude(state, resultOutcome(res), time.Now())
	if err := s.gh.UpdateCheckRun(ctx, pr.InstallationID, pr.Owner, pr.Repo, state.CheckRunID, run); err != nil {
		return PRState{}, History{}, fmt.Errorf("conclude check run %d: %w", state.CheckRunID, err)
	}
	if !reconciles(res.Verdict) {
		return next, history, nil
	}
	if err := s.store.SavePR(ctx, next, history); err != nil {
		return PRState{}, History{}, fmt.Errorf("save state: %w", err)
	}
	posted, unsaved, err := s.postComments(ctx, next, pr, res.Verdict, changed)
	backoff := s.collectBackoff
	for attempt := 1; attempt < postAttempts && err != nil; attempt++ {
		select {
		case <-ctx.Done():
			return PRState{}, History{}, errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		// A failed attempt may have retired comments and saved state before
		// creating; reconciling from the pre-post state again would orphan them.
		latest, lerr := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
		if lerr != nil {
			err = errors.Join(err, fmt.Errorf("load state to retry posting comments: %w", lerr))
			break
		}
		posted, unsaved, err = s.postComments(ctx, latest, pr, res.Verdict, changed)
	}
	if err != nil {
		// A check claiming proposals that were never posted is worse than a neutral
		// one the user can re-run, so re-arm the run for the deadline sweep.
		if rerr := s.rearm(ctx, pr, state.Run); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}
	return posted, unsaved, err
}

// rearm restores run on the stored state, which keeps whatever comment IDs the
// failed posts saved.
func (s *Service) rearm(ctx context.Context, pr PullRequest, run *AwaitingRun) error {
	latest, err := s.store.LoadPR(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return fmt.Errorf("load state to re-arm run: %w", err)
	}
	latest.Run = run
	if err := s.store.SavePR(ctx, latest, History{}); err != nil {
		return fmt.Errorf("save re-armed state: %w", err)
	}
	return nil
}

// failureCause is the fixed one-line text for err; error text from a model or
// provider never reaches GitHub.
func failureCause(err error) string {
	var failed *review.FailedError
	if errors.As(err, &failed) {
		switch failed.Cause {
		case review.CauseProvider:
			return "The model provider returned an error."
		case review.CauseTimeout:
			return "The analysis timed out."
		case review.CauseLimit:
			return "The analysis hit its step or token limit."
		case review.CauseTooManyCandidates:
			return "Too many docs cover the changed files."
		case review.CauseClone:
			return "Reading the repository failed."
		case review.CauseInternal:
			return "The analysis failed unexpectedly."
		}
	}
	return "The analysis failed unexpectedly."
}

// HandleRunCompleted concludes the check run of the analysis run rc reports,
// if it is the one the pull request awaits; any other completion is ignored.
func (s *Service) HandleRunCompleted(ctx context.Context, rc RunCompleted) error {
	state, err := s.store.LoadPR(ctx, rc.Owner, rc.Repo, rc.Number)
	if err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: load state: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	if !MatchesRun(state, rc) {
		return nil
	}

	outcome, err := s.collect(ctx, state, rc)
	if err != nil && outcome.Failed == nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}

	pr := state.pullRequest()
	if outcome.Failed != nil {
		// err is the detail behind the fixed cause; it goes to the job log only.
		if cerr := s.concludeFailed(ctx, state, pr, History{}, outcome); cerr != nil {
			err = errors.Join(err, cerr)
		}
		if err != nil {
			return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
		}
		return nil
	}

	var changed []review.ChangedFile
	if reconciles(outcome.Result.Verdict) {
		changed, err = s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
		if err != nil {
			return fmt.Errorf("handle run %d of %s/%s#%d: list changed files: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
		}
	}
	next, unsaved, err := s.concludeResult(ctx, state, pr, *outcome.Result, changed)
	if err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}
	if err := s.store.SavePR(ctx, next, unsaved); err != nil {
		return fmt.Errorf("handle run %d of %s/%s#%d: save state: %w", rc.RunID, rc.Owner, rc.Repo, rc.Number, err)
	}

	return nil
}

// HandleDeadline concludes the check run neutral if the run identified by
// nonce is still awaited for ref and overdue at now; otherwise it does nothing.
func (s *Service) HandleDeadline(ctx context.Context, ref PRRef, nonce string, now time.Time) error {
	state, err := s.store.LoadPR(ctx, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: load state: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	if !Overdue(state, now) || state.Run.Nonce != nonce {
		return nil
	}

	cause := "The analysis did not report a result before the deadline."
	if state.Run.RunID != 0 {
		cause = "The pollux-agent workflow run did not report a result before the deadline."
	}
	pr := state.pullRequest()
	if err := s.concludeFailed(ctx, state, pr, History{}, failedOutcome(cause)); err != nil {
		return fmt.Errorf("handle deadline of %s/%s#%d: %w", ref.Owner, ref.Repo, ref.Number, err)
	}
	return nil
}

// runFailureCause is the fixed one-line text for a workflow run that did not succeed.
func runFailureCause(conclusion string) string {
	switch conclusion {
	case "failure":
		return "The pollux-agent workflow run failed."
	case "cancelled":
		return "The pollux-agent workflow run was cancelled."
	case "timed_out":
		return "The pollux-agent workflow run timed out."
	default:
		return "The pollux-agent workflow run did not succeed."
	}
}

// collect returns the outcome of the run rc reports. When the run failed or its
// result is unusable it returns a failed outcome with a fixed cause together
// with the detail error, which must not reach GitHub.
func (s *Service) collect(ctx context.Context, state PRState, rc RunCompleted) (Outcome, error) {
	if s.runners.Actions == nil {
		return Outcome{}, errors.New("collect result: no Actions runner configured")
	}

	completion := review.Completion{
		InstallationID: state.InstallationID,
		Owner:          state.Owner,
		Repo:           state.Repo,
		Number:         state.Number,
		HeadSHA:        state.HeadSHA,
		BaseSHA:        state.Run.BaseSHA,
		RunID:          state.Run.RunID,
		Nonce:          state.Run.Nonce,
	}
	var invalid *review.InvalidResultError
	failed := rc.Conclusion != "success"

	var result review.Result
	var err error
	if failed {
		result, err = s.runners.Actions.Collect(ctx, completion)
	} else {
		result, err = collectWithRetry(ctx, s.collectBackoff, func(ctx context.Context) (review.Result, error) {
			return s.runners.Actions.Collect(ctx, completion)
		})
	}
	if err != nil && !failed && ctx.Err() != nil {
		return Outcome{}, fmt.Errorf("collect result: %w", err)
	}
	switch {
	case failed:
		return failedOutcome(runFailureCause(rc.Conclusion)), err
	case errors.As(err, &invalid):
		return failedOutcome("The pollux-agent workflow run returned an invalid result."), err
	case err != nil:
		return failedOutcome("Pollux could not read the workflow run's result."), err
	default:
		return resultOutcome(result), nil
	}
}

// collectWithRetry returns try's result, retrying a transient error with a
// doubling backoff up to collectAttempts tries. It stops at once on
// *review.InvalidResultError or when ctx ends.
func collectWithRetry[T any](ctx context.Context, backoff time.Duration, try func(context.Context) (T, error)) (T, error) {
	var invalid *review.InvalidResultError
	result, err := try(ctx)
	for attempt := 1; attempt < collectAttempts && err != nil && !errors.As(err, &invalid); attempt++ {
		select {
		case <-ctx.Done():
			var zero T
			return zero, fmt.Errorf("wait to retry collect: %w", ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		result, err = try(ctx)
	}
	return result, err
}

// postComments lists the PR's comments when there is anything to reconcile,
// performs Reconcile's writes, and records created comment IDs and URLs in the
// returned state, which is prev otherwise unchanged. It saves Reconcile's
// history with the state before the first create, and returns it for the
// caller's save when it made none.
func (s *Service) postComments(ctx context.Context, prev PRState, pr PullRequest, verdict review.Verdict, changed []review.ChangedFile) (PRState, History, error) {
	proposals, _ := verdict.(review.Proposals)
	if len(prev.Proposals) == 0 && prev.SummaryCommentID == 0 && len(proposals) == 0 {
		return prev, History{}, nil
	}
	existing, err := s.gh.ListComments(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return PRState{}, History{}, fmt.Errorf("list comments: %w", err)
	}

	next, writes, history := Reconcile(prev, pr, verdict, changed, existing)
	// GitHub orders comments by creation time, so a new summary is created
	// before the review comments to sit above them, then edited with their
	// links. The retires stay first: once state is saved without their IDs, a
	// crash must not leave a live marker a later run could adopt.
	if i := slices.IndexFunc(writes, func(w CommentWrite) bool { return w.Summary && w.ID == 0 }); i >= 0 && len(writes) > 1 {
		first := writes[i]
		rest := slices.Delete(slices.Clone(writes), i, i+1)
		at := slices.IndexFunc(rest, func(w CommentWrite) bool { return !w.Resolve })
		if at < 0 {
			at = len(rest)
		}
		writes = append(slices.Insert(rest, at, first), CommentWrite{Summary: true})
	}
	// Saving before the first create means a run that stops after posting
	// leaves state behind, so the next run lists comments and adopts them by marker.
	saved := false
	for _, w := range writes {
		if w.ID == 0 && !saved {
			if err := s.store.SavePR(ctx, next, history); err != nil {
				return PRState{}, History{}, fmt.Errorf("save state before creating comments: %w", err)
			}
			saved, history = true, History{}
		}
		switch {
		case w.Summary:
			err = s.writeSummary(ctx, pr, &next, w)
		case w.ID == 0:
			err = s.createProposalComment(ctx, pr, &next, w)
		default:
			err = s.editProposalComment(ctx, pr, next.Proposals[w.Index], w)
		}
		if err != nil {
			return PRState{}, History{}, err
		}
	}
	return next, history, nil
}

// postFailureSummary writes the summary comment with prev's failure cause, leaving proposals untouched.
func (s *Service) postFailureSummary(ctx context.Context, prev PRState, pr PullRequest) (PRState, error) {
	existing, err := s.gh.ListComments(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return PRState{}, fmt.Errorf("list comments: %w", err)
	}
	next, w := ReconcileFailure(prev, existing)
	if err := s.writeSummary(ctx, pr, &next, w); err != nil {
		return PRState{}, err
	}
	return next, nil
}

func (s *Service) editProposalComment(ctx context.Context, pr PullRequest, ps ProposalState, w CommentWrite) error {
	if err := s.gh.EditReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, w.ID, w.Body); err != nil {
		return fmt.Errorf("edit review comment for %s: %w", ps.DocPath, err)
	}
	if w.Resolve {
		if err := s.gh.ResolveReviewThread(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, w.ID); err != nil {
			s.log.WarnContext(ctx, "resolve review thread failed", "owner", pr.Owner, "repo", pr.Repo, "number", pr.Number, "comment_id", w.ID, "error", err)
		}
	}
	return nil
}

func (s *Service) createProposalComment(ctx context.Context, pr PullRequest, next *PRState, w CommentWrite) error {
	ps := &next.Proposals[w.Index]
	c, err := s.gh.CreateReviewComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, w.Review)
	if err != nil {
		return fmt.Errorf("create review comment for %s: %w", ps.DocPath, err)
	}
	ps.CommentID, ps.CommentURL = c.ID, c.URL
	return nil
}

func (s *Service) writeSummary(ctx context.Context, pr PullRequest, next *PRState, w CommentWrite) error {
	body := renderSummary(*next)
	if id := cmp.Or(w.ID, next.SummaryCommentID); id != 0 {
		if err := s.gh.EditIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, id, body); err != nil {
			return fmt.Errorf("edit summary comment: %w", err)
		}
		return nil
	}
	c, err := s.gh.CreateIssueComment(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number, body)
	if err != nil {
		return fmt.Errorf("create summary comment: %w", err)
	}
	next.SummaryCommentID = c.ID
	return nil
}

// runnerSelection names which runner HandlePullRequest uses.
type runnerSelection int

const (
	runnerNone runnerSelection = iota
	runnerActions
	runnerServer
)

// selectRunner picks the runner a repo uses: the Actions runner when the
// workflow is present, else the server runner, else none. A nil runner in
// the chosen slot counts as none; it never falls back to the other runner.
func selectRunner(hasWorkflow bool, runners Runners) runnerSelection {
	if hasWorkflow {
		if runners.Actions == nil {
			return runnerNone
		}
		return runnerActions
	}
	if runners.Server == nil {
		return runnerNone
	}
	return runnerServer
}

type startOutcome struct {
	started   review.Started
	changed   []review.ChangedFile
	mergeBase string
}

func (s *Service) start(ctx context.Context, runner review.Runner, pr PullRequest) (startOutcome, error) {
	changed, err := s.gh.ListChangedFiles(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return startOutcome{}, fmt.Errorf("list changed files: %w", err)
	}
	if limit, ok := oversized(changed); ok {
		return startOutcome{}, &tooLargeError{limit: limit}
	}

	mergeBase, err := s.gh.MergeBase(ctx, pr.InstallationID, pr.Owner, pr.Repo, pr.BaseSHA, pr.HeadSHA)
	if err != nil {
		return startOutcome{}, fmt.Errorf("find merge base: %w", err)
	}

	started, err := runner.Start(ctx, review.Request{
		InstallationID: pr.InstallationID,
		Owner:          pr.Owner,
		Repo:           pr.Repo,
		Number:         pr.Number,
		BaseSHA:        mergeBase,
		HeadSHA:        pr.HeadSHA,
		ChangedFiles:   changed,
	})
	if err != nil {
		return startOutcome{}, fmt.Errorf("start analysis: %w", err)
	}
	return startOutcome{started: started, changed: changed, mergeBase: mergeBase}, nil
}

// reconciles reports whether a verdict concludes the analysis, so that its
// proposals belong in comments; an empty proposal list is a failed analysis.
func reconciles(v review.Verdict) bool {
	switch v := v.(type) {
	case review.NoImpact:
		return true
	case review.Proposals:
		return len(v) > 0
	}
	return false
}

// proposalsSummary lists one line per proposal, stopping before the check run
// summary limit so truncate never cuts through a code span.
func proposalsSummary(proposals review.Proposals) string {
	const moreRoom = 64
	var b strings.Builder
	for i, p := range proposals {
		line := fmt.Sprintf("- %s: %s", codeSpan(p.DocPath), inertProse(p.Reason))
		if b.Len()+len(line)+1 > maxSummaryBytes-moreRoom {
			fmt.Fprintf(&b, "\n- … and %d more", len(proposals)-i)
			break
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(line)
	}
	return b.String()
}
