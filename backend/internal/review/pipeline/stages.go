package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/mrkizildag/pollux-agent/backend/internal/docs"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/finalize"
)

// run is one analysis's shared model state: the workspace, the logger, the
// token meter, the prompt fence, and the PR's combined diff every prompt carries.
type run struct {
	s     *Sync
	ws    Workspace
	log   *slog.Logger
	meter *Meter
	fence fence
	patch string
}

func (s *Sync) newRun(ws Workspace, req review.Request) (run, error) {
	f, err := newFence()
	if err != nil {
		return run{}, err
	}
	return run{
		s:     s,
		ws:    ws,
		log:   s.log.With("repo", req.Owner+"/"+req.Repo, "pr", req.Number, "head_sha", req.HeadSHA),
		meter: NewMeter(s.limits.Tokens),
		fence: f,
		patch: combinedPatch(req.ChangedFiles),
	}, nil
}

// oneLine collapses s onto a single line and truncates it to max bytes. It
// caps log and error text; no-impact reasons use finalize.NoImpactReason.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "..."
	}
	return s
}

// The verdict flags are pointers so a reply that omits them is an error, not
// a silent "no".
type triageVerdict struct {
	Impacted *bool  `json:"impacted"`
	Reason   string `json:"reason"`
}

type newDocVerdict struct {
	Needed *bool  `json:"needed"`
	Reason string `json:"reason"`
}

type verifyVerdict struct {
	Supported *bool  `json:"supported"`
	Reason    string `json:"reason"`
}

// decodeVerdict decodes the first JSON object in reply into v, tolerating
// code fences and surrounding prose.
func decodeVerdict(reply string, v any) error {
	start := strings.IndexByte(reply, '{')
	if start < 0 {
		return fmt.Errorf("decode verdict from reply %q: no JSON object", oneLine(reply, 200))
	}
	if err := json.NewDecoder(strings.NewReader(reply[start:])).Decode(v); err != nil {
		return fmt.Errorf("decode verdict from reply %q: %w", oneLine(reply, 200), err)
	}
	return nil
}

// ask sends one tool-less prompt to the judge and decodes the JSON verdict in
// the reply into v. It returns the reply text for error messages.
func (r run) ask(ctx context.Context, kind, system, prompt string, v any) (string, error) {
	reply, err := r.s.judge.Ask(ctx, Question{Kind: kind, System: system, Prompt: prompt, Log: r.log}, r.meter)
	if err != nil {
		return "", fmt.Errorf("ask judge: %w", err)
	}
	if err := decodeVerdict(reply, v); err != nil {
		return reply, fmt.Errorf("%w: %w", ErrProvider, err)
	}
	return reply, nil
}

// triageAll triages every candidate, returning the impacted docs and a
// "path: reason" line for each doc left out.
func (r run) triageAll(ctx context.Context, candidates []docs.Doc) (impacted []docs.Doc, reasons []string, err error) {
	for _, d := range candidates {
		isImpacted, why, err := r.triage(ctx, d)
		if err != nil {
			return nil, nil, fmt.Errorf("triage %s: %w", d.Path, err)
		}
		if isImpacted {
			impacted = append(impacted, d)
		} else {
			reasons = append(reasons, d.Path+": "+why)
		}
	}
	return impacted, reasons, nil
}

// triage runs one judge call for doc, returning whether the PR's diff makes it
// stale and why.
func (r run) triage(ctx context.Context, doc docs.Doc) (impacted bool, reason string, err error) {
	var v triageVerdict
	reply, err := r.ask(ctx, "triage", triageSystemPrompt, triageUserPrompt(r.fence, doc, r.patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Impacted == nil {
		return false, "", fmt.Errorf("%w: triage verdict has no \"impacted\" field in reply %q", ErrProvider, oneLine(reply, 200))
	}
	return *v.Impacted, v.Reason, nil
}

// newDocAllowed decides, against the head's docs index, whether the uncovered
// files need a new doc, and why not when they don't.
func (r run) newDocAllowed(ctx context.Context, uncovered []string) (needed bool, reason string, err error) {
	readme, err := r.headReadme(ctx)
	if err != nil {
		return false, "", fmt.Errorf("read docs/README.md: %w", err)
	}
	var v newDocVerdict
	reply, err := r.ask(ctx, "new_doc", newDocSystemPrompt, newDocUserPrompt(r.fence, readme, uncovered, r.patch), &v)
	if err != nil {
		return false, "", fmt.Errorf("decide new doc: %w", err)
	}
	if v.Needed == nil {
		return false, "", fmt.Errorf("decide new doc: %w: new-doc verdict has no \"needed\" field in reply %q", ErrProvider, oneLine(reply, 200))
	}
	return *v.Needed, v.Reason, nil
}

// headReadme reads docs/README.md at the head, empty when it is not a regular
// file.
func (r run) headReadme(ctx context.Context) (string, error) {
	src, _, err := finalize.ReadDoc(ctx, r.ws, "docs/README.md")
	if err != nil {
		return "", fmt.Errorf("read head README: %w", err)
	}
	return string(src), nil
}

// verifyAll verifies every proposal, returning the supported ones and a
// "path: reason" line for each rejected one.
func (r run) verifyAll(ctx context.Context, proposals []review.Proposal) (kept []review.Proposal, rejected []string, err error) {
	for _, p := range proposals {
		supported, why, err := r.verify(ctx, p)
		if err != nil {
			return nil, nil, fmt.Errorf("verify proposal %s: %w", p.DocPath, err)
		}
		if supported {
			kept = append(kept, p)
		} else {
			rejected = append(rejected, p.DocPath+": "+why)
		}
	}
	return kept, rejected, nil
}

// verify asks the judge whether p is supported by the patch, given the doc
// section p replaces.
func (r run) verify(ctx context.Context, p review.Proposal) (supported bool, reason string, err error) {
	section := p.Original
	if p.Section == "" {
		section = "(new doc)"
	}

	var v verifyVerdict
	reply, err := r.ask(ctx, "verify", verifySystemPrompt, verifyUserPrompt(r.fence, p, section, r.patch), &v)
	if err != nil {
		return false, "", err
	}
	if v.Supported == nil {
		return false, "", fmt.Errorf("%w: verify verdict has no \"supported\" field in reply %q", ErrProvider, oneLine(reply, 200))
	}
	return *v.Supported, v.Reason, nil
}

// submitProposalsArgs is the argument shape of the submit_proposals finishing
// tool: an object wrapping the array, since function-calling parameters must
// be a JSON Schema object.
type submitProposalsArgs struct {
	Proposals []review.Proposal `json:"proposals"`
}

// draft runs the backend task that drafts proposals and finalizes each
// submission against the workspace's head under rules. It returns the final
// proposals and the model that drafted them.
func (r run) draft(ctx context.Context, rules finalize.Rules, prompt draftPrompt) ([]review.Proposal, string, error) {
	finish, err := submitProposalsFinish()
	if err != nil {
		return nil, "", err
	}

	var finalized []review.Proposal
	out, err := r.s.backend.Run(ctx, r.ws, Task{
		System: draftSystemPrompt(),
		Prompt: prompt.user(r.fence, r.patch),
		Finish: finish,
		Accept: func(ctx context.Context, raw json.RawMessage) (feedback, fatal error) {
			var parsed submitProposalsArgs
			if err := json.Unmarshal(raw, &parsed); err != nil {
				return fmt.Errorf("decode submit_proposals arguments: %w", err), nil
			}
			out, problems, err := finalize.Proposals(ctx, r.ws, rules, parsed.Proposals)
			if err != nil {
				return nil, fmt.Errorf("finalize proposals: %w", err)
			}
			if len(problems) > 0 {
				return problems, nil
			}
			finalized = out
			return nil, nil
		},
		Limits: r.s.limits,
		Meter:  r.meter,
		Log:    r.log,
	})
	if err != nil {
		return nil, "", fmt.Errorf("draft proposals: %w", err)
	}
	return finalized, out.Model, nil
}

func submitProposalsFinish() (Finish, error) {
	proposalSchema, err := review.ProposalSchema()
	if err != nil {
		return Finish{}, fmt.Errorf("build submit_proposals schema: %w", err)
	}

	schema, err := json.Marshal(struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}{
		Type: "object",
		Properties: map[string]any{
			"proposals": map[string]any{
				"type":  "array",
				"items": json.RawMessage(proposalSchema),
			},
		},
		Required: []string{"proposals"},
	})
	if err != nil {
		return Finish{}, fmt.Errorf("marshal submit_proposals schema: %w", err)
	}

	return Finish{
		Name:        "submit_proposals",
		Description: "Submit the final list of doc proposals for this PR. An empty list means no doc needs to change.",
		Schema:      schema,
	}, nil
}
