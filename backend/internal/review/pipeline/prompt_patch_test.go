package pipeline_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestCombinedPatchCapCutsAtALineEnd(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString("@@ -0,0 +1,9000 @@ f")
	for i := range 9000 {
		fmt.Fprintf(&b, "\n+line %04d END", i)
	}
	got := pipeline.CombinedPatch([]review.ChangedFile{{Path: "big.go", Patch: b.String()}})

	before, _, found := strings.Cut(got, "\n(patch truncated")
	if !found {
		t.Fatalf("combined patch was not truncated:\n%.200s", got)
	}
	if last := before[strings.LastIndexByte(before, '\n')+1:]; !strings.HasSuffix(last, " END") {
		t.Errorf("last line before the truncation note = %q, want a whole numbered line", last)
	}
}
