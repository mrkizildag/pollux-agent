package pipeline_test

import (
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/input"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

// The hunk listing has one line per changed file, in order; removed files and
// files without hunks list no ranges.
func TestHunkRanges(t *testing.T) {
	t.Parallel()

	req := review.Request{BaseSHA: "base", ChangedFiles: []review.ChangedFile{
		{Path: "b.go", Hunks: []review.LineRange{{Start: 1, End: 3}, {Start: 9, End: 9}}},
		{Path: "gone.go", Removed: true},
		{Path: `we"ird.go`, Hunks: []review.LineRange{{Start: 4, End: 5}}},
	}}
	in := input.New(req, basedocs.Selection{})

	want := "\"b.go\": 1-3, 9-9\n\"gone.go\": \n\"we\\\"ird.go\": 4-5\n"
	if got := pipeline.HunkRanges(in.Files); got != want {
		t.Errorf("HunkRanges() = %q, want %q", got, want)
	}
	if got := pipeline.HunkRanges(input.New(review.Request{}, basedocs.Selection{}).Files); got != "" {
		t.Errorf("HunkRanges(no files) = %q, want empty", got)
	}
}
