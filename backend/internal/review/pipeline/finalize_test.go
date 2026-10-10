package pipeline_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func TestStart_SectionWithHashesIsNormalized(t *testing.T) {
	t.Parallel()

	p := proposalFor("docs/x.md", 2)
	p["section"] = "## X"
	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(p)}})
	if got := a.proposals(t, 1)[0].Section; got != "X" {
		t.Fatalf("Section = %q, want \"X\"", got)
	}
}

func TestStart_ProposalCarriesOriginalSectionAndLines(t *testing.T) {
	t.Parallel()

	const frontmatter = "---\ntitle: X\nsummary: Describes X.\ncovers:\n  - main.go\n---\n"
	const body = "# X\n\n## Mid\nmid body\n\n## Last\nlast body"

	tests := []struct {
		name      string
		doc       string
		section   string
		want      string
		wantLines review.LineRange
	}{
		{
			name:      "middle section followed by a blank line counts frontmatter",
			doc:       frontmatter + body + "\n",
			section:   "Mid",
			want:      "## Mid\nmid body\n\n",
			wantLines: review.LineRange{Start: 9, End: 11},
		},
		{
			name:      "last section with trailing newline",
			doc:       frontmatter + body + "\n",
			section:   "Last",
			want:      "## Last\nlast body\n",
			wantLines: review.LineRange{Start: 12, End: 13},
		},
		{
			name:      "last section without trailing newline",
			doc:       frontmatter + body,
			section:   "Last",
			want:      "## Last\nlast body",
			wantLines: review.LineRange{Start: 12, End: 13},
		},
		{
			name:      "heading written with leading hashes",
			doc:       frontmatter + body + "\n",
			section:   "## Mid",
			want:      "## Mid\nmid body\n\n",
			wantLines: review.LineRange{Start: 9, End: 11},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			proposal := proposalFor("docs/x.md", 2)
			proposal["section"] = tc.section
			ws := xWorkspace().headFile("docs/x.md", tc.doc)
			a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
				&fakeBackend{submissions: []json.RawMessage{submit(proposal)}})
			got := a.proposals(t, 1)[0]
			if got.Original != tc.want || got.Lines != tc.wantLines {
				t.Fatalf("Original, Lines = %q, %+v, want %q, %+v", got.Original, got.Lines, tc.want, tc.wantLines)
			}

			docLines := strings.Split(strings.TrimSuffix(tc.doc, "\n"), "\n")
			replaced := strings.Join(docLines[got.Lines.Start-1:got.Lines.End], "\n")
			if replaced != strings.TrimSuffix(got.Original, "\n") {
				t.Errorf("doc lines %d-%d = %q, want them to equal Original %q", got.Lines.Start, got.Lines.End, replaced, got.Original)
			}
		})
	}
}

func TestStart_SectionEditOfDocWithBrokenFrontmatterAtHeadCarriesOriginal(t *testing.T) {
	t.Parallel()

	ws := xWorkspace().headFile("docs/x.md", "---\ntitle: [unclosed\n---\n# Top\n\n## X\nold behavior.\n")
	a := mustAnalyze(t, ws, newJudge().on(pipeline.KindTriage, triage(true)).on(pipeline.KindVerify, verify(true)),
		&fakeBackend{submissions: []json.RawMessage{submit(proposalFor("docs/x.md", 2))}})
	p := a.proposals(t, 1)[0]
	if !strings.Contains(p.Original, "old behavior.") || p.Lines == (review.LineRange{}) {
		t.Errorf("Original, Lines = %q, %+v; want the section's current text", p.Original, p.Lines)
	}
}

func TestStart_DraftThatProposesNothingIsNoImpactFromTheDraftModel(t *testing.T) {
	t.Parallel()

	a := mustAnalyze(t, xWorkspace(), newJudge().on(pipeline.KindTriage, triage(true)),
		&fakeBackend{submissions: []json.RawMessage{submit()}})
	if got := a.noImpact(t).Reason; got != "model proposed no doc changes" {
		t.Errorf("Reason = %q, want %q", got, "model proposed no doc changes")
	}
	if a.result.Model != "draft-model" {
		t.Errorf("Model = %q, want draft-model", a.result.Model)
	}
}
