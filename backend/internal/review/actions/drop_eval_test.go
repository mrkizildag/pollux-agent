package actions_test

import (
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestEvalMixedOutsideHunkAnchor(t *testing.T) {
	t.Parallel()
	bad := validProposal()
	bad["anchor"] = map[string]any{"file": "main.go", "line": 99}
	api := &fakeAPI{
		artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{validProposal(), bad}}}),
		changed:  []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}},
		files:    map[string][]byte{"docs/a.md": []byte(usageDoc)},
	}
	got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	kept, ok := got.Verdict.(review.Proposals)
	if !ok || len(kept) != 1 || len(got.Dropped) != 1 || got.Dropped[0].Index != 1 || !strings.Contains(got.Dropped[0].Reason, "anchor.line 99") {
		t.Fatalf("mixed outside-hunk result = %+v", got)
	}
}
