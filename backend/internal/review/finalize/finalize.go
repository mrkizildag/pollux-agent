// Package finalize is the step both analysis runners run on a model's raw
// proposals: normalize, validate, and fill each section's current text
// from the PR's head commit.
package finalize

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/basedocs"
)

const (
	maxReasonLen  = 300
	maxProblemLen = 4096 // backstop; the heading list and commentable ranges the model needs must fit.
	maxQuotedLen  = 200  // cap on each model-supplied path or section interpolated into a problem.

	// MaxProposals is the most proposals Proposals accepts in one batch.
	MaxProposals = 20
)

// Kind is what a Head finds at one path.
type Kind int

const (
	Missing Kind = iota
	Dir
	Other // a file of any size, a symlink, a submodule
)

// Head reads the PR's head commit one path at a time; Proposals owns the
// rules about parent directories.
type Head interface {
	// Stat reports what is at path itself; it must not follow a symlink at path.
	Stat(ctx context.Context, path string) (Kind, error)
	// ReadFile returns a regular file; ok is false when there is none or it is over docs.MaxDocBytes.
	ReadFile(ctx context.Context, path string) (src []byte, ok bool, err error)
}

// Rules is what the proposals are checked against.
type Rules struct {
	Changed []review.ChangedFile
	// Selection nil means no base selection: proposals get Proposal.Validate
	// only, and every new doc is refused.
	Selection *basedocs.Selection
	Repo      string // owner/repo, for doc-link checks
	// AllowNewDoc permits new docs; it requires a non-nil Selection, since
	// their covers cannot be checked without one.
	AllowNewDoc bool
}

// Problem is one reason proposal Index cannot be accepted. Its text is one
// line and bounded. It may quote the model-supplied paths, section names and
// link targets and the headings of the head doc, but never the proposal's
// Content field.
type Problem struct {
	Index int
	Err   error
}

// Problems is every problem found in one batch.
type Problems []Problem

func (ps Problems) Error() string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = fmt.Sprintf("proposal %d: %v", p.Index, p.Err)
	}
	return strings.Join(parts, "; ")
}

type readFile struct {
	doc docs.Doc
	ok  bool
}

// Proposals returns the finalized proposals and every problem found (nil when
// none). Output remains aligned with raw, including rejected proposals, so callers
// must inspect Problems before accepting entries. err is set only when a head
// read fails, which is transient, or when rules allow new docs without a
// Selection. More than MaxProposals proposals yield one problem and no reads.
func Proposals(ctx context.Context, head Head, rules Rules, raw []review.Proposal) ([]review.Proposal, Problems, error) {
	if rules.AllowNewDoc && rules.Selection == nil {
		return nil, nil, errors.New("finalize rules: AllowNewDoc requires a Selection")
	}
	if len(raw) > MaxProposals {
		return nil, Problems{{Index: MaxProposals, Err: fmt.Errorf("too many proposals: %d, max %d", len(raw), MaxProposals)}}, nil
	}

	out := make([]review.Proposal, len(raw))
	read := map[string]readFile{}
	view := &headView{head: head, stats: map[string]Kind{}}

	var problems Problems

	for i, p := range raw {
		fail := func(format string, args ...any) {
			problems = append(problems, Problem{Index: i, Err: errors.New(review.OneLine(fmt.Sprintf(format, args...), maxProblemLen))})
		}

		out[i] = p

		var err error
		if rules.Selection != nil {
			err = rules.Selection.ValidateProposal(p, rules.Changed, rules.Repo)
		} else {
			err = p.Validate(rules.Changed)
		}
		if err != nil {
			fail("%v", err)
			continue
		}

		p.Section = docs.NormalizeHeading(p.Section)
		out[i] = p

		if p.Section == "" {
			if !rules.AllowNewDoc {
				fail("doc_path %q: new docs are not allowed here", quoted(p.DocPath))
				continue
			}
			pk, err := view.parents(ctx, p.DocPath)
			if err != nil {
				return nil, nil, err
			}
			if pk == Other {
				fail("doc_path %q: already exists at head", quoted(p.DocPath))
				continue
			}
			if pk == Dir {
				k, err := view.stat(ctx, p.DocPath)
				if err != nil {
					return nil, nil, err
				}
				if k != Missing {
					fail("doc_path %q: already exists at head", quoted(p.DocPath))
				}
			}
			continue
		}

		f, seen := read[p.DocPath]
		if !seen {
			src, ok, err := view.readDoc(ctx, p.DocPath)
			if err != nil {
				return nil, nil, err
			}
			f = readFile{ok: ok}
			if ok {
				f.doc = docs.ParseBody(p.DocPath, src)
			}
			read[p.DocPath] = f
		}
		if !f.ok {
			fail("doc_path %q: no such doc at head, or it is over %d bytes", quoted(p.DocPath), docs.MaxDocBytes)
			continue
		}

		switch n := countHeading(f.doc, p.Section); {
		case n == 0:
			fail("section %q: no such heading in %q; its headings are %s", quoted(p.Section), quoted(p.DocPath), headings(f.doc))
		case n > 1:
			fail("section %q: ambiguous, %d headings of %q match; its headings are %s", quoted(p.Section), n, quoted(p.DocPath), headings(f.doc))
		default:
			p.Original, p.Lines.Start, p.Lines.End, _ = f.doc.SectionSpan(p.Section)
			out[i] = p
		}
	}

	return out, problems, nil
}

// NoImpactReason collapses s to one trimmed line of at most maxReasonLen bytes.
func NoImpactReason(s string) string {
	return review.OneLine(s, maxReasonLen)
}

func quoted(s string) string {
	return review.OneLine(s, maxQuotedLen)
}

func countHeading(d docs.Doc, heading string) int {
	n := 0
	for _, s := range d.Sections {
		if s.Level != 0 && s.Heading == heading {
			n++
		}
	}
	return n
}

func headings(d docs.Doc) string {
	var hs []string
	for _, s := range d.Sections {
		if s.Level != 0 {
			hs = append(hs, fmt.Sprintf("%q", s.Heading))
		}
	}
	if len(hs) == 0 {
		return "(none)"
	}
	return strings.Join(hs, ", ")
}

// ReadDoc reads docPath at head the way a section edit's doc is read: only when
// every parent is a directory, so a doc is never reached through a symlink or a
// file. ok is false when there is no such regular file within docs.MaxDocBytes.
func ReadDoc(ctx context.Context, head Head, docPath string) (src []byte, ok bool, err error) {
	return (&headView{head: head, stats: map[string]Kind{}}).readDoc(ctx, docPath)
}

// headView caches Stat results for one pass over a head.
type headView struct {
	head  Head
	stats map[string]Kind
}

func (v *headView) stat(ctx context.Context, path string) (Kind, error) {
	if k, ok := v.stats[path]; ok {
		return k, nil
	}
	k, err := v.head.Stat(ctx, path)
	if err != nil {
		return Missing, fmt.Errorf("check %s at head: %w", path, err)
	}
	v.stats[path] = k
	return k, nil
}

// parents walks docPath's parent directories top-down and stops at the first
// that is not a directory, so nothing is looked up below a symlink or a file.
// It returns Dir when every parent is one.
func (v *headView) parents(ctx context.Context, docPath string) (Kind, error) {
	segs := strings.Split(docPath, "/")
	for i := 1; i < len(segs); i++ {
		k, err := v.stat(ctx, strings.Join(segs[:i], "/"))
		if err != nil || k != Dir {
			return k, err
		}
	}
	return Dir, nil
}

func (v *headView) readDoc(ctx context.Context, docPath string) ([]byte, bool, error) {
	pk, err := v.parents(ctx, docPath)
	if err != nil || pk != Dir {
		return nil, false, err
	}
	src, ok, err := v.head.ReadFile(ctx, docPath)
	if err != nil {
		return nil, false, fmt.Errorf("read %s at head: %w", docPath, err)
	}
	return src, ok, nil
}
