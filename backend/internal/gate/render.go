package gate

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const summaryMarker = "<!-- pollux-agent:summary -->"

const forkApplyNote = "Apply is not available: this pull request comes from a fork the bot cannot push to."

func proposalMarker(id string) string {
	return "<!-- pollux-agent:proposal:" + id + " -->"
}

func supersededMarker(id string) string {
	return "<!-- pollux-agent:superseded:" + id + " -->"
}

// proposalComment is the review comment for p: a suggestion on the doc's own
// lines when they lie within one head-side hunk of the PR diff, else the
// checkbox variant on the anchor's line, or on the anchor's file when that line
// is outside the file's hunks.
func proposalComment(headSHA, id string, p review.Proposal, changed []review.ChangedFile, fork bool) ReviewComment {
	if suggestable(p, changed) {
		rc := ReviewComment{CommitSHA: headSHA, Path: p.DocPath, Line: p.Lines.End, Body: renderSuggestion(id, p, fork)}
		if p.Lines.Start != p.Lines.End {
			rc.StartLine = p.Lines.Start
		}
		return rc
	}
	rc := ReviewComment{CommitSHA: headSHA, Path: p.Anchor.File, Line: p.Anchor.Line, Body: renderCheckbox(id, p, fork)}
	if !inHunk(p.Anchor.File, p.Anchor.Line, changed) {
		rc.Line, rc.File = 0, true
	}
	return rc
}

func inHunk(path string, line int, changed []review.ChangedFile) bool {
	for _, f := range changed {
		if f.Path != path || f.Removed {
			continue
		}
		for _, h := range f.Hunks {
			if line >= h.Start && line <= h.End {
				return true
			}
		}
	}
	return false
}

func suggestable(p review.Proposal, changed []review.ChangedFile) bool {
	if p.Section == "" || p.Lines.Start < 1 || p.Lines.End < p.Lines.Start {
		return false
	}
	for _, f := range changed {
		if f.Path != p.DocPath {
			continue
		}
		for _, h := range f.Hunks {
			if p.Lines.Start >= h.Start && p.Lines.End <= h.End {
				return true
			}
		}
	}
	return false
}

func renderSuggestion(id string, p review.Proposal, fork bool) string {
	// Lines covers the section's trailing blank lines; restate them so the
	// suggestion does not remove the gap before the next heading.
	original := strings.TrimRight(p.Original, "\n")
	blank := max(len(strings.TrimPrefix(p.Original, original))-1, 0)
	content := strings.TrimRight(p.Content, "\n") + "\n" + strings.Repeat("\n", blank)
	fence := fenceFor(content)
	body := proposalMarker(id) + "\n\n" + inertProse(p.Reason) + "\n\n" + fence + "suggestion\n" + content + fence + "\n"
	if fork {
		body += "\n" + forkApplyNote + "\n"
	}
	return body
}

// isSuperseded reports whether the first line of body is a superseded marker.
func isSuperseded(body string) bool {
	first, _, _ := strings.Cut(body, "\n")
	first = strings.TrimRight(first, "\r")
	return strings.HasPrefix(first, "<!-- pollux-agent:superseded:") && strings.HasSuffix(first, " -->")
}

// renderSuperseded swaps the marker of a retired proposal comment, so the gate
// never adopts it as the live comment again, and closes the suggestion fence
// into a plain one so GitHub no longer offers to commit the stale text.
func renderSuperseded(id, old string) string {
	body := strings.Replace(old, proposalMarker(id), supersededMarker(id), 1)
	loc := suggestionFence.FindStringSubmatchIndex(body)
	if loc == nil {
		return body
	}
	return body[:loc[3]] + body[loc[1]:]
}

// suggestionFence matches the opening line of a suggestion block; group 1 is its backtick fence.
var suggestionFence = regexp.MustCompile("(?m)^(`{3,})suggestion$")

// renderCheckbox is the checkbox-variant review comment body: the edit as a
// diff of the section's old lines against the proposed ones.
func renderCheckbox(id string, p review.Proposal, fork bool) string {
	var diff strings.Builder
	if p.Original != "" {
		writePrefixed(&diff, "-", p.Original)
	}
	writePrefixed(&diff, "+", p.Content)

	var b strings.Builder
	b.WriteString(proposalMarker(id))
	b.WriteString("\n\n")
	b.WriteString(inertProse(p.Reason))
	b.WriteString("\n\n")
	b.WriteString(proposalTarget(p))
	b.WriteString("\n\n")
	fence := fenceFor(diff.String())
	b.WriteString(fence + "diff\n" + diff.String() + fence + "\n")
	if p.IndexEntry != "" {
		fmt.Fprintf(&b, "\nIndex entry: %s\n", codeSpan(p.IndexEntry))
	}
	if fork {
		b.WriteString("\n" + forkApplyNote + "\n")
	} else {
		b.WriteString("\n" + checkbox(false, applyLabel) + "\n")
	}
	return b.String()
}

// checkbox is the markdown line for a box labelled label.
func checkbox(ticked bool, label string) string {
	if ticked {
		return "- [x] " + label
	}
	return "- [ ] " + label
}

// setCheckbox sets the box labelled label in body to ticked; ok is false when
// body has no such box in the other state.
func setCheckbox(body, label string, ticked bool) (string, bool) {
	from, to := checkbox(!ticked, label), checkbox(ticked, label)
	lines := strings.Split(body, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == from {
			lines[i] = strings.Replace(l, from, to, 1)
			return strings.Join(lines, "\n"), true
		}
	}
	return body, false
}

func proposalTarget(p review.Proposal) string {
	if p.Section == "" {
		return "New doc: " + codeSpan(p.DocPath)
	}
	return codeSpan(p.DocPath) + ", section " + codeSpan(p.Section)
}

func writePrefixed(b *strings.Builder, prefix, text string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(text, "\n"), "\n") {
		b.WriteString(prefix + line + "\n")
	}
}

// fenceFor returns a backtick fence longer than any backtick run in body, so
// proposed content containing code fences cannot close the diff block early.
func fenceFor(body string) string {
	return strings.Repeat("`", max(3, longestBacktickRun(body)+1))
}

func longestBacktickRun(body string) int {
	longest := 0
	for i := 0; i < len(body); {
		if body[i] != '`' {
			i++
			continue
		}
		n := backtickRunAt(body, i)
		longest = max(longest, n)
		i += n
	}
	return longest
}

// maxProseRunes caps a model-written reason before it is escaped.
const maxProseRunes = 1000

// oneLine collapses whitespace to single spaces and drops control, bidi and
// zero-width-space characters, which can reorder or hide the text around them.
// Dropping them before the collapse keeps them from leaving a leading space.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		if unicode.IsControl(r) || r == '\u200b' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' {
			return -1
		}
		return r
	}, s)), " ")
}

// codeSpan renders model-written identifier text as one inline code span: one
// line, delimited by a backtick run longer than any inside it, so nothing in it
// is interpreted as Markdown.
func codeSpan(text string) string {
	text = oneLine(text)
	if strings.HasPrefix(text, "`") || strings.HasSuffix(text, "`") {
		text = " " + text + " "
	}
	delim := strings.Repeat("`", longestBacktickRun(text)+1)
	return delim + text + delim
}

// tableCodeSpan is codeSpan for a table cell, where GFM splits cells on `|`
// before it parses code spans. A zero-width space between a backslash and the
// pipe's escape keeps the pipe escaped even under a scanner that pairs `\\`.
func tableCodeSpan(text string) string {
	span := codeSpan(text)
	var b strings.Builder
	for i := 0; i < len(span); i++ {
		if span[i] == '|' {
			if i > 0 && span[i-1] == '\\' {
				b.WriteString("\u200b")
			}
			b.WriteByte('\\')
		}
		b.WriteByte(span[i])
	}
	return b.String()
}

// inertProse renders model-written prose as one line of Markdown that cannot
// mention, link or autolink, embed an image, open HTML, decode an entity or
// start a block, or link an issue or pull request (#N, GH-N). Balanced inline code spans stay as written; every other
// backtick is escaped.
func inertProse(text string) string {
	text = oneLine(text)
	if r := []rune(text); len(r) > maxProseRunes {
		text = strings.TrimSpace(string(r[:maxProseRunes-1])) + "…"
	}
	var b strings.Builder
	for i := 0; i < len(text); {
		n := 1
		switch c := text[i]; c {
		case '`':
			n = backtickRunAt(text, i)
			if end := closingBacktickRun(text, i+n, n); end >= 0 {
				b.WriteString(text[i : end+n])
				n = end + n - i
			} else {
				b.WriteString(strings.Repeat("\\`", n))
			}
		case '<':
			b.WriteString("&lt;")
		case '&':
			b.WriteString("&amp;")
		case '@':
			b.WriteString("@\u200b")
		case ':':
			if strings.HasPrefix(text[i:], "://") {
				b.WriteString(":\u200b")
			} else {
				b.WriteByte(c)
			}
		case 'w', 'W':
			if len(text) >= i+4 && strings.EqualFold(text[i:i+4], "www.") {
				b.WriteString(text[i:i+3] + "\u200b")
				n = 3
			} else {
				b.WriteByte(c)
			}
		case '#':
			b.WriteByte(c)
			if i+1 < len(text) && isDigit(text[i+1]) {
				b.WriteString("\u200b")
			}
		case 'G', 'g':
			if len(text) > i+3 && strings.EqualFold(text[i:i+3], "gh-") && isDigit(text[i+3]) {
				b.WriteString(text[i:i+2] + "\u200b")
				n = 2
			} else {
				b.WriteByte(c)
			}
		case '(':
			if i > 0 && text[i-1] == ']' {
				b.WriteString("\\(")
			} else {
				b.WriteByte(c)
			}
		case '\\', '[', ']':
			b.WriteString("\\" + string(c))
		default:
			b.WriteByte(c)
		}
		i += n
	}
	return escapeBlockStart(b.String())
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func backtickRunAt(text string, i int) int {
	n := 0
	for i+n < len(text) && text[i+n] == '`' {
		n++
	}
	return n
}

// closingBacktickRun returns the index of the first backtick run of exactly n
// at or after from, or -1.
func closingBacktickRun(text string, from, n int) int {
	for j := from; j < len(text); {
		if text[j] != '`' {
			j++
			continue
		}
		m := backtickRunAt(text, j)
		if m == n {
			return j
		}
		j += m
	}
	return -1
}

// escapeBlockStart keeps a line from opening as a heading, quote, list item,
// checkbox, rule or fence.
func escapeBlockStart(line string) string {
	if line == "" {
		return line
	}
	if strings.IndexByte("#>-+*=~_", line[0]) >= 0 {
		return "\\" + line
	}
	d := 0
	for d < len(line) && isDigit(line[d]) {
		d++
	}
	if d > 0 && d < len(line) && (line[d] == '.' || line[d] == ')') {
		return line[:d] + "\\" + line[d:]
	}
	return line
}

// droppedNotice renders diagnostic text as one inert line and never truncates
// inside escaped prose or a model-supplied code span.
func droppedNotice(dropped []review.DroppedProposal) string {
	dropped = boundedDropped(dropped)
	if len(dropped) == 0 {
		return ""
	}
	noun := "proposals"
	if len(dropped) == 1 {
		noun = "proposal"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Dropped %d %s: ", len(dropped), noun)
	for i, d := range dropped {
		reason := inertProse(d.Reason)
		if b.Len()+len(reason)+2 > 4096-64 {
			fmt.Fprintf(&b, "; … and %d more reasons", len(dropped)-i)
			break
		}
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(reason)
	}
	return b.String()
}

// renderSummary is the summary comment body: a heading (the failure cause when
// the last analysis failed, else the open proposal count), one row per proposal
// in state, then the PR-wide checkboxes redrawn from state. Re-run is offered
// for a failure only; Apply all is never drawn ticked.
func renderSummary(state PRState) string {
	var b strings.Builder
	b.WriteString(summaryMarker)
	b.WriteString("\n\n")
	applied, open := 0, 0
	for _, p := range state.Proposals {
		switch p.State {
		case ProposalApplied:
			applied++
		case ProposalOpen:
			open++
		case ProposalOutdated:
		}
	}
	if state.FailureCause != "" {
		b.WriteString("**Analysis failed:** " + state.FailureCause + "\n\n")
	} else {
		noun := "updates"
		if open == 1 {
			noun = "update"
		}
		switch {
		case skipActive(state) && open == 0:
			b.WriteString("**pollux-agent**: the check is skipped.\n\n")
		case skipActive(state):
			fmt.Fprintf(&b, "**pollux-agent** proposed %d doc %s; the check is skipped.\n\n", open, noun)
		case open == 0 && applied > 0:
			b.WriteString("**pollux-agent**: every proposed doc update is applied.\n\n")
		case open == 0 && len(state.Proposals) > 0:
			b.WriteString("**pollux-agent**: no doc updates are needed now; earlier proposals are outdated.\n\n")
		case open == 0:
			b.WriteString("**pollux-agent**: no doc updates are needed.\n\n")
		default:
			fmt.Fprintf(&b, "**pollux-agent** proposes %d doc %s.\n\n", open, noun)
		}
	}
	if notice := droppedNotice(state.Dropped); notice != "" {
		b.WriteString(notice + "\n\n")
	}
	if len(state.Proposals) > 0 {
		b.WriteString("| Doc | Section | Comment | State |\n| --- | --- | --- | --- |\n")
	}
	skipped := skipActive(state)
	for _, p := range state.Proposals {
		section := "(new doc)"
		if p.Section != "" {
			section = tableCodeSpan(p.Section)
		}
		status := string(p.State)
		switch {
		case p.State == ProposalApplied:
			status = fmt.Sprintf("applied (%s)", shortSHA(p.AppliedSHA))
		case p.State == ProposalOpen && skipped:
			status = "skipped"
		}
		link := "-"
		if p.CommentURL != "" {
			link = fmt.Sprintf("[view](%s)", p.CommentURL)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", tableCodeSpan(p.DocPath), section, link, status)
	}

	if len(state.Proposals) > 0 {
		b.WriteString("\n")
		switch {
		case state.Fork:
			b.WriteString("Apply all is not available: this pull request comes from a fork the bot cannot push to.\n")
		case applied > 0 && open == 0:
			b.WriteString("✅ All proposals applied.\n")
		case open == 0:
			// Every proposal is outdated: there is nothing left to apply.
		default:
			b.WriteString(checkbox(false, applyAllLabel) + "\n")
		}
	}
	active := state.Skip
	if !skipActive(state) {
		active = nil
	}
	pending := state.PendingSkip
	b.WriteString(checkbox(scopeIs(SkipCommit, pending, active), skipCommitLabel) + "\n")
	b.WriteString(checkbox(scopeIs(SkipPR, pending, active), skipPRLabel) + "\n")
	if state.FailureCause != "" {
		b.WriteString(checkbox(false, rerunLabel) + "\n")
	}

	if active != nil {
		fmt.Fprintf(&b, "\nSkipped by @%s for this %s: %s\n", active.User, active.Scope.noun(), inertProse(active.Reason))
	}
	if state.PendingSkip != nil {
		fmt.Fprintf(&b, "\nWaiting for @%s to post the reason as a new comment on this PR.\n", state.PendingSkip.User)
	}
	fmt.Fprintf(&b, "\nCommands: `%[1]s %[2]s`, `%[1]s %[3]s <reason>`, `%[1]s %[4]s <reason>`.\n", commandPrefix, applyCommand, skipCommand, skipPRCommand)
	return b.String()
}

func scopeIs(scope SkipScope, pending *SkipAsk, active *Skip) bool {
	return pending != nil && pending.Scope == scope || active != nil && active.Scope == scope
}
