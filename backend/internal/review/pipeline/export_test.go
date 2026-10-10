package pipeline

import (
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
)

// CombinedPatch exposes combinedPatch for tests of the prompt's diff block.
func CombinedPatch(changed []review.ChangedFile) string {
	return combinedPatch(changed)
}

// HunkRanges exposes hunkRanges for tests of the prompt's anchor listing.
func HunkRanges(files []input.File) string {
	return hunkRanges(files)
}

// DraftSystemPrompt exposes draftSystemPrompt for tests of the shared rules.
func DraftSystemPrompt() string {
	return draftSystemPrompt()
}

// ScaffoldSystemPrompt exposes scaffoldSystemPrompt for tests of the shared rules.
func ScaffoldSystemPrompt() string {
	return scaffoldSystemPrompt()
}

// The stage prompts, exposed for tests of the shared rules.
const (
	TriageSystemPrompt = triageSystemPrompt
	NewDocSystemPrompt = newDocSystemPrompt
	VerifySystemPrompt = verifySystemPrompt
	UntrustedMarkers   = untrustedMarkers
)
