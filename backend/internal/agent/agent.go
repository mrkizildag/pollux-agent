// Package agent runs the model-driven tool-use loop shared by the server
// runner and the scaffold command: the model reads files through os.Root
// until it calls a caller-supplied finishing tool.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

// ErrStepLimit means the loop reached Task.MaxSteps without the model calling
// Task.Finish.
var ErrStepLimit = errors.New("agent: step limit reached before the model finished")

// ErrTokenBudget means a Budget's Charge pushed its usage past the cap.
var ErrTokenBudget = errors.New("agent: token budget exceeded")

// ErrDeadline means the run's context deadline passed. Cancellation is not a
// deadline and never wraps this error.
var ErrDeadline = errors.New("agent: deadline exceeded")

// ErrMalformed means the model returned maxEmptyReplies consecutive replies
// with neither text nor tool calls.
var ErrMalformed = errors.New("agent: model returned no usable reply")

const maxEmptyReplies = 3

// Task describes one agent run.
type Task struct {
	Model    string
	System   string
	Prompt   string
	Root     *os.Root
	Finish   llm.Tool
	Accept   func(json.RawMessage) error
	MaxSteps int
	// Log receives one record per tool call and one summary record per run;
	// nil discards them.
	Log *slog.Logger
}

// Stats is what a Run consumed.
type Stats struct {
	Steps        int
	InputTokens  int
	OutputTokens int
}

// Budget bounds the total tokens a Run may spend.
type Budget struct {
	max   int
	total llm.Usage
}

// NewBudget returns a Budget that allows up to maxTokens total input and
// output tokens.
func NewBudget(maxTokens int) *Budget {
	return &Budget{max: maxTokens}
}

// Charge adds u to the budget's running total, returning ErrTokenBudget once
// the total exceeds the budget's cap.
func (b *Budget) Charge(u llm.Usage) error {
	b.total.InputTokens += u.InputTokens
	b.total.OutputTokens += u.OutputTokens
	b.total.CacheReadTokens += u.CacheReadTokens
	b.total.CacheWriteTokens += u.CacheWriteTokens
	if used := b.total.InputTokens + b.total.OutputTokens; used > b.max {
		return fmt.Errorf("used %d tokens, budget %d: %w", used, b.max, ErrTokenBudget)
	}
	return nil
}

// Charger is what a run bills each model reply's usage to; a Budget is one.
// Its error ends the run and is returned as is.
type Charger interface {
	Charge(u llm.Usage) error
}

// Usage is everything charged to the budget so far, cache tokens included.
func (b *Budget) Usage() llm.Usage { return b.total }

// Run drives m through t's conversation: on each step it offers read_file, grep,
// list_dir and t.Finish, executes any other tool call against t.Root, and ends when the
// model calls t.Finish with arguments t.Accept accepts. A t.Accept error is
// reported back to the model as a tool error, and the loop continues.
func Run(ctx context.Context, m llm.Model, t Task, b Charger) (json.RawMessage, Stats, error) {
	if t.Log == nil {
		t.Log = slog.New(slog.DiscardHandler)
	}
	start := time.Now()
	raw, stats, err := run(ctx, m, t, b)
	outcome := "finished"
	if err != nil {
		outcome = err.Error()
	}
	t.Log.Info("agent run done", "steps", stats.Steps, "input_tokens", stats.InputTokens,
		"output_tokens", stats.OutputTokens, "duration", time.Since(start).Round(time.Millisecond), "outcome", outcome)
	return raw, stats, err
}

func run(ctx context.Context, m llm.Model, t Task, b Charger) (json.RawMessage, Stats, error) {
	tools := append(readTools(), t.Finish)
	messages := []llm.Message{{Role: llm.RoleUser, Text: t.Prompt}}
	var stats Stats
	empty := 0

	for step := 0; step < t.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return nil, stats, ctxError(step+1, err)
		}
		stats.Steps++

		resp, err := m.Complete(ctx, llm.Request{
			Model:    t.Model,
			System:   t.System,
			Messages: messages,
			Tools:    tools,
		})
		if err != nil {
			return nil, stats, ctxError(step+1, err)
		}

		stats.InputTokens += resp.Usage.InputTokens
		stats.OutputTokens += resp.Usage.OutputTokens
		if err := b.Charge(resp.Usage); err != nil {
			return nil, stats, fmt.Errorf("charge step %d: %w", step+1, err)
		}

		for _, call := range resp.ToolCalls {
			t.Log.Info("agent step", "step", stats.Steps, "tool", call.Name, "arg", argSummary(call),
				"input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens)
		}

		if len(resp.ToolCalls) == 0 {
			if resp.Text == "" {
				empty++
				if empty >= maxEmptyReplies {
					return nil, stats, fmt.Errorf("agent: %d consecutive empty replies by step %d: %w", empty, stats.Steps, ErrMalformed)
				}
			} else {
				empty = 0
				messages = append(messages, llm.Message{Role: llm.RoleAssistant, Text: resp.Text})
			}
			messages = append(messages, llm.Message{Role: llm.RoleUser, Text: "Continue by calling a tool, or call " + t.Finish.Name + " when you are done."})
			continue
		}
		empty = 0

		messages = append(messages, llm.Message{Role: llm.RoleAssistant, Text: resp.Text, ToolCalls: resp.ToolCalls})

		results := make([]llm.ToolResult, 0, len(resp.ToolCalls))
		var finishArgs json.RawMessage
		finished := false

		for _, call := range resp.ToolCalls {
			if call.Name != t.Finish.Name {
				results = append(results, callTool(t.Root, call))
				continue
			}

			if err := t.Accept(call.Args); err != nil {
				results = append(results, llm.ToolResult{CallID: call.ID, Content: err.Error(), IsError: true})
				continue
			}
			finishArgs = call.Args
			finished = true
			results = append(results, llm.ToolResult{CallID: call.ID, Content: "accepted"})
		}

		if finished {
			return finishArgs, stats, nil
		}

		messages = append(messages, llm.Message{Role: llm.RoleUser, ToolResults: results})
	}

	return nil, stats, fmt.Errorf("agent: after %d steps: %w", stats.Steps, ErrStepLimit)
}

// ctxError wraps err from step n, adding ErrDeadline when a deadline passed.
func ctxError(n int, err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("agent: step %d: %w: %w", n, ErrDeadline, err)
	}
	return fmt.Errorf("agent: step %d: %w", n, err)
}

const maxArgSummary = 120

// argSummary is the path or pattern a read tool call targets, truncated; it
// is empty for other tools so submitted content never reaches the log.
func argSummary(call llm.ToolCall) string {
	var a struct {
		Path    string `json:"path"`
		Pattern string `json:"pattern"`
	}
	switch call.Name {
	case readFileToolName, listDirToolName, grepToolName:
		if json.Unmarshal(call.Args, &a) != nil {
			return ""
		}
	default:
		return ""
	}
	s := strings.Join(strings.Fields(a.Pattern+" "+a.Path), " ")
	if len(s) > maxArgSummary {
		s = strings.ToValidUTF8(s[:maxArgSummary], "") + "..."
	}
	return s
}
