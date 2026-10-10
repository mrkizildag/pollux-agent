//go:build eval

package llmrunner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/config"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const evalHTTPTimeout = 60 * time.Second

var unsafeNameChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

type reportProposal struct {
	DocPath string `json:"doc_path"`
	Section string `json:"section"`
	Reason  string `json:"reason"`
	Content string `json:"content"`
}

type reportRun struct {
	Run            int              `json:"run"`
	Verdict        string           `json:"verdict,omitempty"`
	NoImpactReason string           `json:"no_impact_reason,omitempty"`
	Proposals      []reportProposal `json:"proposals,omitempty"`
	// Models are the models the run used, when the runner reports them.
	Models     []string       `json:"models,omitempty"`
	Score      runScore       `json:"score"`
	Judge      []docJudgement `json:"judge,omitempty"`
	Error      string         `json:"error,omitempty"`
	Cause      string         `json:"cause,omitempty"`
	DurationMS int64          `json:"duration_ms"`
}

type reportCase struct {
	ID     string      `json:"id"`
	Source string      `json:"source"`
	Expect string      `json:"expect"`
	Runs   []reportRun `json:"runs"`
}

// totals are nil when the cases run have nothing to measure.
type totals struct {
	PassRate         *float64 `json:"pass_rate"`
	ImpactPassRate   *float64 `json:"impact_pass_rate"`
	NoImpactAccuracy *float64 `json:"no_impact_accuracy"`
	MeanFactCoverage *float64 `json:"mean_fact_coverage"`
}

type report struct {
	StartedAt string `json:"started_at"`
	// Config records the runner kind; its secrets marshal as "[redacted]".
	Config config.Eval  `json:"config"`
	Cases  []reportCase `json:"cases"`
	Totals totals       `json:"totals"`
}

func ratio(n, d float64) *float64 {
	if d == 0 {
		return nil
	}
	r := n / d
	return &r
}

func computeTotals(cases []reportCase) totals {
	var runs, passes, impactRuns, impactPasses, noImpactRuns, noImpactPasses int
	var coverage float64
	for _, c := range cases {
		for _, r := range c.Runs {
			runs++
			if r.Score.Pass {
				passes++
			}
			switch c.Expect {
			case verdictProposals:
				impactRuns++
				coverage += r.Score.FactCoverage
				if r.Score.Pass {
					impactPasses++
				}
			case verdictNoImpact:
				noImpactRuns++
				if r.Score.Pass {
					noImpactPasses++
				}
			}
		}
	}
	return totals{
		PassRate:         ratio(float64(passes), float64(runs)),
		ImpactPassRate:   ratio(float64(impactPasses), float64(impactRuns)),
		NoImpactAccuracy: ratio(float64(noImpactPasses), float64(noImpactRuns)),
		MeanFactCoverage: ratio(coverage, float64(impactRuns)),
	}
}

func failureReasons(expect string, r reportRun) []string {
	var reasons []string
	switch {
	case r.Error != "" && r.Cause != "":
		reasons = append(reasons, "error: "+r.Cause)
	case r.Error != "":
		reasons = append(reasons, "error")
	case r.Score.Pass:
	case !r.Score.VerdictOK:
		reasons = append(reasons, "wrong verdict")
	case expect == verdictEither:
		if !r.Score.SectionsOK {
			reasons = append(reasons, "wrong section")
		}
		if r.Score.Precision < 1 {
			reasons = append(reasons, "unexpected doc")
		}
	case expect == verdictProposals:
		for _, f := range []struct {
			bad  bool
			text string
		}{
			{r.Score.Recall < 1, "missing doc"},
			{!r.Score.SectionsOK, "wrong section"},
			{r.Score.Precision < 1, "unexpected doc"},
			{r.Score.FactCoverage < 1, "facts missing"},
			{len(r.Score.Contradictions) > 0, "contradiction"},
		} {
			if f.bad {
				reasons = append(reasons, f.text)
			}
		}
	}
	for _, j := range r.Judge {
		if j.Error != "" {
			reasons = append(reasons, "judge error")
		}
	}
	return reasons
}

func mean(runs []reportRun, f func(runScore) float64) float64 {
	sum := 0.0
	for _, r := range runs {
		sum += f(r.Score)
	}
	return sum / float64(len(runs))
}

func pct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.1f%%", *p*100)
}

func pctDelta(cur, prev *float64) string {
	if cur == nil || prev == nil {
		return ""
	}
	return fmt.Sprintf(" (%+.1f pp)", (*cur-*prev)*100)
}

// judgeName is the judge model, or Claude Code's default when none is set.
func judgeName(model string) string {
	if model == "" {
		return "claude"
	}
	return model
}

// usedModels lists the distinct models the runs reported, markdown-quoted.
func usedModels(rep report) string {
	var models []string
	for _, c := range rep.Cases {
		for _, r := range c.Runs {
			models = append(models, r.Models...)
		}
	}
	slices.Sort(models)
	models = slices.Compact(models)
	if len(models) == 0 {
		return "n/a"
	}
	for i, m := range models {
		models[i] = "`" + m + "`"
	}
	return strings.Join(models, ", ")
}

// renderSummary formats rep as markdown; failed runs score zero throughout.
func renderSummary(rep report, prev *totals, prevName string) string {
	var b strings.Builder
	if rep.Config.Runner == config.EvalRunnerActions {
		fmt.Fprintf(&b, "# Eval summary\n\nActions runner, models %s, judge `%s`, %d runs per case.\n\n", usedModels(rep), judgeName(rep.Config.JudgeModel), rep.Config.Runs)
	} else {
		fmt.Fprintf(&b, "# Eval summary\n\nModel `%s`, triage `%s`, judge `%s`, %d runs per case.\n\n", rep.Config.LLM.Model, rep.Config.LLM.TriageModel, rep.Config.JudgeModel, rep.Config.Runs)
	}

	b.WriteString("| Case | Expect | Pass | Recall | Precision | Fact coverage | Errored | Failures |\n|---|---|---|---|---|---|---|---|\n")
	for _, c := range rep.Cases {
		passed, errored := 0, 0
		var reasons []string
		for _, r := range c.Runs {
			if r.Score.Pass {
				passed++
			}
			if r.Error != "" {
				errored++
			}
			reasons = append(reasons, failureReasons(c.Expect, r)...)
		}
		slices.Sort(reasons)
		recall, precision, coverage := "-", "-", "-"
		if c.Expect == verdictProposals {
			recall = fmt.Sprintf("%.2f", mean(c.Runs, func(s runScore) float64 { return s.Recall }))
			precision = fmt.Sprintf("%.2f", mean(c.Runs, func(s runScore) float64 { return s.Precision }))
			coverage = fmt.Sprintf("%.2f", mean(c.Runs, func(s runScore) float64 { return s.FactCoverage }))
		}
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %s | %s | %s | %d | %s |\n",
			c.ID, c.Expect, passed, len(c.Runs), recall, precision, coverage, errored, strings.Join(slices.Compact(reasons), ", "))
	}

	b.WriteString("\n## Totals\n\n")
	if prev != nil {
		fmt.Fprintf(&b, "Deltas are against %s.\n\n", prevName)
	} else {
		prev = &totals{}
	}
	for _, row := range []struct {
		name      string
		cur, prev *float64
	}{
		{"Overall pass rate", rep.Totals.PassRate, prev.PassRate},
		{"Impact-case pass rate", rep.Totals.ImpactPassRate, prev.ImpactPassRate},
		{"No-impact accuracy", rep.Totals.NoImpactAccuracy, prev.NoImpactAccuracy},
		{"Mean fact coverage", rep.Totals.MeanFactCoverage, prev.MeanFactCoverage},
	} {
		fmt.Fprintf(&b, "- %s: %s%s\n", row.name, pct(row.cur), pctDelta(row.cur, row.prev))
	}
	return b.String()
}

// previousReport returns the totals of the newest report.json under
// resultsRoot, or nil if there is none. Directory names start with a UTC
// timestamp, so name order is age order. Only totals are decoded: the config's
// secrets are written redacted and cannot be read back.
func previousReport(resultsRoot string) (*totals, string, error) {
	paths, err := filepath.Glob(filepath.Join(resultsRoot, "*", "report.json"))
	if err != nil {
		return nil, "", fmt.Errorf("list previous reports: %w", err)
	}
	if len(paths) == 0 {
		return nil, "", nil
	}
	slices.Sort(paths)
	latest := paths[len(paths)-1]
	data, err := os.ReadFile(latest) //nolint:gosec // latest comes from globbing the results directory
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", latest, err)
	}
	var prev struct {
		Totals totals `json:"totals"`
	}
	if err := json.Unmarshal(data, &prev); err != nil {
		return nil, "", fmt.Errorf("decode %s: %w", latest, err)
	}
	return &prev.Totals, filepath.Base(filepath.Dir(latest)), nil
}

// attemptOutcome is one runner attempt on a case: its finished result or the
// runner's failure, and the models it reports having used.
type attemptOutcome struct {
	Result review.Result
	Models []string
	Err    error
}

// attemptFunc runs the runner under test once. Only harness problems are its
// error; a runner failure is the outcome's Err.
type attemptFunc func(ctx context.Context, c evalCase, in evalInput, runNo int) (attemptOutcome, error)

// serverAttempt runs the server runner, logging each run under logDir.
func serverAttempt(cfg config.Eval, m llm.Model, logDir string) attemptFunc {
	return func(ctx context.Context, c evalCase, in evalInput, runNo int) (out attemptOutcome, err error) {
		logPath := filepath.Join(logDir, fmt.Sprintf("%s-%d.jsonl", c.ID, runNo))
		logFile, err := os.Create(logPath) //nolint:gosec // the path is built from a validated case id inside the results directory
		if err != nil {
			return attemptOutcome{}, fmt.Errorf("create run log %s: %w", logPath, err)
		}
		defer func() {
			if cerr := logFile.Close(); cerr != nil && err == nil {
				err = fmt.Errorf("close run log %s: %w", logPath, cerr)
			}
		}()

		runner := newRunnerWith(m, cfg.LLM.TriageModel, cfg.LLM.Model, slog.New(slog.NewJSONHandler(logFile, nil)))
		runner.SetRemote(in.Remote)

		started, runErr := runner.Start(ctx, in.Request)
		res, isResult := started.(review.Result)
		if runErr == nil && !isResult {
			runErr = fmt.Errorf("runner returned %T, want review.Result", started)
		}
		return attemptOutcome{Result: res, Err: runErr}, nil
	}
}

// execRun runs the runner under test once on in and scores the result. A runner
// failure is recorded in the returned run; only harness problems are errors.
func execRun(ctx context.Context, attempt attemptFunc, judge judgeFunc, c evalCase, in evalInput, runNo int) (reportRun, error) {
	start := time.Now()
	out, err := attempt(ctx, c, in, runNo)
	if err != nil {
		return reportRun{}, err
	}
	run := reportRun{Run: runNo, DurationMS: time.Since(start).Milliseconds(), Models: out.Models}

	if out.Err != nil {
		run.Error = out.Err.Error()
		var failed *review.FailedError
		if errors.As(out.Err, &failed) {
			run.Cause = string(failed.Cause)
		}
		return run, nil
	}

	res := out.Result
	run.Verdict = verdictKind(res.Verdict)
	switch v := res.Verdict.(type) {
	case review.NoImpact:
		run.NoImpactReason = v.Reason
	case review.Proposals:
		for _, p := range v {
			run.Proposals = append(run.Proposals, reportProposal{DocPath: p.DocPath, Section: p.Section, Reason: p.Reason, Content: p.Content})
		}
	}
	run.Score, run.Judge = scoreRun(ctx, judge, c, res)
	return run, nil
}

func TestEval(t *testing.T) {
	cfg, err := config.LoadEval()
	if err != nil {
		t.Fatalf("load eval config: %v", err)
	}

	ctx := t.Context()
	root, err := evalRepoRoot(ctx)
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	cases, err := loadEvalCases(filepath.Join(root, "eval", "cases"), cfg.Cases)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}

	resultsRoot := filepath.Join(root, "eval", "results")
	started := time.Now().UTC()
	nameSuffix := cfg.LLM.Model
	if cfg.Runner == config.EvalRunnerActions {
		nameSuffix = "actions-" + judgeName(cfg.JudgeModel)
	}
	resultsDir := filepath.Join(resultsRoot, started.Format("20060102-150405")+"-"+unsafeNameChars.ReplaceAllString(nameSuffix, "-"))
	logDir := filepath.Join(resultsDir, "logs")
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatalf("create results dir: %v", err)
	}

	var attempt attemptFunc
	var judge judgeFunc
	switch cfg.Runner {
	case config.EvalRunnerServer:
		model, err := llm.New(string(cfg.LLM.Provider), &http.Client{Timeout: evalHTTPTimeout}, cfg.LLM.BaseURL, cfg.LLM.APIKey.Reveal())
		if err != nil {
			t.Fatalf("build LLM model: %v", err)
		}
		attempt, judge = serverAttempt(cfg, model, logDir), modelJudge(model, cfg.JudgeModel)
	case config.EvalRunnerActions:
		if err := missingTools("bash", "jq", "git", "claude"); err != nil {
			t.Fatalf("EVAL_RUNNER=actions: %v", err)
		}
		sandbox := claudeSandbox{path: cfg.Path, oauthToken: cfg.ClaudeOAuthToken, apiKey: cfg.AnthropicAPIKey}
		attempt, judge = actionsAttempt(root, logDir, sandbox), claudeJudge(sandbox, cfg.JudgeModel)
	default:
		t.Fatalf("EVAL_RUNNER: unknown runner %q", cfg.Runner)
	}
	prev, prevName, err := previousReport(resultsRoot)
	if err != nil {
		t.Logf("no comparison with a previous report: %v", err)
	}

	inputs := make([]evalInput, len(cases))
	for i, c := range cases {
		if inputs[i], err = buildEvalInput(ctx, root, t.TempDir(), c); err != nil {
			t.Fatalf("build input: %v", err)
		}
	}

	runs := make([][]reportRun, len(cases))
	harnessErrs := make([][]error, len(cases))
	for i := range cases {
		runs[i] = make([]reportRun, cfg.Runs)
		harnessErrs[i] = make([]error, cfg.Runs)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.Parallel)
	for i, c := range cases {
		for n := range cfg.Runs {
			sem <- struct{}{}
			wg.Go(func() {
				defer func() { <-sem }()
				runs[i][n], harnessErrs[i][n] = execRun(ctx, attempt, judge, c, inputs[i], n+1)
				t.Logf("%s run %d: pass=%v err=%q", c.ID, n+1, runs[i][n].Score.Pass, runs[i][n].Error)
			})
		}
	}
	wg.Wait()
	if err := errors.Join(slices.Concat(harnessErrs...)...); err != nil {
		t.Fatalf("run cases: %v", err)
	}

	rep := report{StartedAt: started.Format(time.RFC3339), Config: cfg}
	for i, c := range cases {
		rep.Cases = append(rep.Cases, reportCase{ID: c.ID, Source: c.Source, Expect: c.Expect.Verdict, Runs: runs[i]})
	}
	rep.Totals = computeTotals(rep.Cases)

	data, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	summary := renderSummary(rep, prev, prevName)
	for name, content := range map[string][]byte{"report.json": data, "summary.md": []byte(summary)} {
		if err := os.WriteFile(filepath.Join(resultsDir, name), content, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	t.Logf("results in %s\n\n%s", resultsDir, summary)
}

func TestEvalPreviousReportReadsRedactedReport(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	rep := report{Config: config.Eval{Runner: config.EvalRunnerActions}, Totals: totals{PassRate: ratio(1, 2)}}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	dir := filepath.Join(root, "20261007-100000-actions-claude")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("create report dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), data, 0o600); err != nil {
		t.Fatalf("write report: %v", err)
	}

	prev, name, err := previousReport(root)
	if err != nil {
		t.Fatalf("previousReport() error = %v", err)
	}
	if prev == nil || prev.PassRate == nil || *prev.PassRate != 0.5 || name != filepath.Base(dir) {
		t.Fatalf("previousReport() = %+v, %q, want pass rate 0.5 from %q", prev, name, filepath.Base(dir))
	}
}
