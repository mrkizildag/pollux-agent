package pipeline_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/instructions"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestServerPromptsStateTheSharedRules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prompt string
		rules  []string
	}{
		{"draft", pipeline.DraftSystemPrompt(), instructions.ReviewRules()},
		{"scaffold", pipeline.ScaffoldSystemPrompt(), instructions.ScaffoldRules()},
		{"triage", pipeline.TriageSystemPrompt, []string{instructions.Threshold, instructions.NoDocNeeded}},
		{"new doc", pipeline.NewDocSystemPrompt, []string{instructions.NewDocWhen, instructions.NoDocNeeded}},
		{"verify", pipeline.VerifySystemPrompt, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			untrusted := strings.Index(tc.prompt, instructions.Untrusted)
			for _, rule := range append([]string{instructions.Untrusted, pipeline.UntrustedMarkers}, tc.rules...) {
				i := strings.Index(tc.prompt, rule)
				if i < 0 {
					t.Errorf("%s prompt lacks rule %q", tc.name, rule)
				}
				if i < untrusted {
					t.Errorf("%s prompt states %q before the untrusted rule", tc.name, rule)
				}
			}
			for _, name := range []string{"JSON schema", "Read, Grep and Glob"} {
				if strings.Contains(tc.prompt, name) {
					t.Errorf("%s prompt names the action's %q", tc.name, name)
				}
			}
		})
	}
}

func TestServerAgentPromptsSayBadSubmissionsComeBack(t *testing.T) {
	t.Parallel()

	for name, prompt := range map[string]string{"draft": pipeline.DraftSystemPrompt(), "scaffold": pipeline.ScaffoldSystemPrompt()} {
		if !strings.Contains(prompt, "is returned with") {
			t.Errorf("%s prompt does not say a bad submission comes back with its problems", name)
		}
	}
}
