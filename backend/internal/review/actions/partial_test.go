package actions_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// rawReviewArtifact deliberately bypasses the ordinary fixture's schema defaults.
func rawReviewArtifact(output string) []byte {
	return []byte(`{"head_sha":"abc","nonce":"n1","claude":{"structured_output":` + output + `}}`)
}

func TestCollectSchemaFailuresRejectValidSibling(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(validProposal())
	if err != nil {
		t.Fatal(err)
	}
	base := string(valid)
	cases := map[string]string{
		"required top level": `{"proposals":[` + base + `]}`,
		"unknown top level":  `{"no_impact_reason":"","proposals":[` + base + `],"extra":true}`,
		"null proposals":     `{"no_impact_reason":"","proposals":null}`,
		"null reason":        `{"no_impact_reason":null,"proposals":[` + base + `]}`,
		"wrong reason type":  `{"no_impact_reason":7,"proposals":[` + base + `]}`,
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing content":        func(p map[string]any) { delete(p, "content") },
		"null content":           func(p map[string]any) { p["content"] = nil },
		"unknown proposal field": func(p map[string]any) { p["extra"] = true },
		"wrong anchor type":      func(p map[string]any) { p["anchor"] = map[string]any{"file": "main.go", "line": "3"} },
		"null anchor":            func(p map[string]any) { p["anchor"] = nil },
	} {
		p := validProposal()
		mutate(p)
		b, e := json.Marshal(p)
		if e != nil {
			t.Fatal(e)
		}
		cases[name] = `{"no_impact_reason":"","proposals":[` + base + `,` + string(b) + `]}`
	}
	fractional := strings.Replace(base, `"line":3`, `"line":9007199254740992.5`, 1)
	cases["fractional numeric rounding"] = `{"no_impact_reason":"","proposals":[` + base + `,` + fractional + `]}`
	for name, output := range cases {
		t.Run(name, func(t *testing.T) {
			api := &fakeAPI{artifact: rawReviewArtifact(output), changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}, files: map[string][]byte{"docs/a.md": []byte(usageDoc)}}
			got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || got.Verdict != nil {
				t.Fatalf("Collect = %+v, %v", got, err)
			}
			if len(api.fileReads) != 0 {
				t.Fatalf("schema failure read head docs: %v", api.fileReads)
			}
		})
	}
}

func TestCollectMixedProposalFailures(t *testing.T) {
	t.Parallel()
	changed := []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}
	for name, mutate := range map[string]func(map[string]any){
		"anchor":                func(p map[string]any) { p["anchor"] = map[string]any{"file": "other.go", "line": 3} },
		"empty content":         func(p map[string]any) { p["content"] = "" },
		"new doc missing index": func(p map[string]any) { p["section"] = "" },
		"new doc covers": func(p map[string]any) {
			p["section"] = ""
			p["doc_path"] = "docs/new.md"
			p["index_entry"] = "- [N](new.md)"
			p["content"] = "---\ntitle: N\nsummary: S\ncovers: [other.go]\n---\n# N\n"
		},
		"new doc frontmatter": func(p map[string]any) {
			p["section"] = ""
			p["doc_path"] = "docs/new.md"
			p["index_entry"] = "- [N](new.md)"
			p["content"] = "# N\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := validProposal()
			mutate(bad)
			api := &fakeAPI{artifact: artifact(t, "abc", "n1", map[string]any{"modelUsage": map[string]any{"model": map[string]any{}}, "usage": map[string]any{"input_tokens": 4}, "structured_output": map[string]any{"proposals": []any{bad, validProposal()}}}), changed: changed, files: map[string][]byte{"docs/a.md": []byte(usageDoc)}, docsAt: map[string]fstest.MapFS{"base": {}}}
			got, err := newRunner(api).Collect(t.Context(), review.Completion{Owner: "o", Repo: "r", HeadSHA: "abc", Nonce: "n1", BaseSHA: "base"})
			if err != nil {
				t.Fatal(err)
			}
			proposals, ok := got.Verdict.(review.Proposals)
			if !ok || len(proposals) != 1 || proposals[0].Original != "## Usage\nold usage\n" || proposals[0].Lines.Start != 9 {
				t.Fatalf("retained = %+v", got)
			}
			if got.Model != "model" || got.Usage == nil || got.Usage.Tokens.Input != 4 || len(got.Dropped) != 1 || got.Dropped[0].Index != 0 {
				t.Fatalf("metadata = %+v", got)
			}
		})
	}
}

func TestCollectUnsafeSiblingRejectsBatch(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"README.md", "/docs/a.md", "docs/../evil.md", "docs/a/../a.md", "docs/a..md", "docs/a\nb.md", "docs/a`b.md"} {
		for _, badFirst := range []bool{true, false} {
			t.Run(path+map[bool]string{true: " first", false: " last"}[badFirst], func(t *testing.T) {
				bad := validProposal()
				bad["doc_path"] = path
				proposals := []any{bad, validProposal()}
				if !badFirst {
					proposals = []any{validProposal(), bad}
				}
				api := &fakeAPI{artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": proposals}}), changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}, files: map[string][]byte{"docs/a.md": []byte(usageDoc)}}
				got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
				var invalid *review.InvalidResultError
				if !errors.As(err, &invalid) || got.Verdict != nil {
					t.Fatalf("Collect = %+v, %v", got, err)
				}
				if len(api.fileReads) != 0 {
					t.Fatalf("unsafe batch read head docs")
				}
			})
		}
	}
}

func TestCollectNoSurvivorsAndOversizedBatch(t *testing.T) {
	t.Parallel()
	for _, count := range []int{1, 21} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			p := validProposal()
			p["section"] = "Absent"
			proposals := make([]any, count)
			for i := range proposals {
				proposals[i] = p
			}
			if count > 20 {
				proposals[0] = validProposal()
			}
			api := &fakeAPI{artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"no_impact_reason": "claim no impact", "proposals": proposals}}), changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}, files: map[string][]byte{"docs/a.md": []byte(usageDoc)}}
			got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
			var invalid *review.InvalidResultError
			if !errors.As(err, &invalid) || got.Verdict != nil {
				t.Fatalf("Collect = %+v, %v", got, err)
			}
			if count > 20 && len(api.fileReads) != 0 {
				t.Fatalf("oversized batch read head docs")
			}
		})
	}
}

func TestCollectTransientReadDiscardsEarlierSuccess(t *testing.T) {
	t.Parallel()
	bad := validProposal()
	bad["doc_path"] = "docs/b.md"
	readErr := errors.New("head unavailable")
	api := &fakeAPI{artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{validProposal(), bad}}}), changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}, files: map[string][]byte{"docs/a.md": []byte(usageDoc), "docs/b.md": []byte(usageDoc)}, fileErr: map[string]error{"docs/b.md": readErr}}
	got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	var invalid *review.InvalidResultError
	if !errors.Is(err, readErr) || errors.As(err, &invalid) || got.Verdict != nil {
		t.Fatalf("Collect = %+v, %v", got, err)
	}
	if api.fileReads["docs/a.md@abc"] != 1 {
		t.Fatal("first valid proposal was not finalized before failed read")
	}
}

func TestCollectRetainsOrderAndBoundsDropReason(t *testing.T) {
	t.Parallel()
	first, last, bad := validProposal(), validProposal(), validProposal()
	first["section"] = "## Usage"
	first["content"] = "first replacement"
	last["content"] = "last replacement"
	bad["section"] = strings.Repeat("missing heading ", 1000)
	bad["content"] = "SECRET PROPOSAL BODY"
	api := &fakeAPI{artifact: artifact(t, "abc", "n1", map[string]any{"structured_output": map[string]any{"proposals": []any{first, bad, last}}}), changed: []review.ChangedFile{{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 5}}}}, files: map[string][]byte{"docs/a.md": []byte(usageDoc)}}
	got, err := newRunner(api).Collect(t.Context(), review.Completion{HeadSHA: "abc", Nonce: "n1"})
	if err != nil {
		t.Fatal(err)
	}
	proposals, ok := got.Verdict.(review.Proposals)
	if !ok {
		t.Fatalf("verdict = %T, want proposals", got.Verdict)
	}
	if len(proposals) != 2 || proposals[0].Content != "first replacement" || proposals[1].Content != "last replacement" || proposals[0].Section != "Usage" || proposals[0].Original != proposals[1].Original {
		t.Fatalf("kept order/normalization = %+v", proposals)
	}
	if len(got.Dropped) != 1 || got.Dropped[0].Index != 1 || len(got.Dropped[0].Reason) > 203 || strings.ContainsAny(got.Dropped[0].Reason, "\r\n") || strings.Contains(got.Dropped[0].Reason, "SECRET PROPOSAL BODY") {
		t.Fatalf("drop diagnostic = %+v", got.Dropped)
	}
}
