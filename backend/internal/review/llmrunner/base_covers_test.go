package llmrunner_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

const mainGoChanged = "package main\n\nfunc main() { println() }\n"

func docWithCovers(covers, body string) string {
	return "---\ntitle: X\nsummary: Describes X.\ncovers:" + covers + "\n---\n# X\n\n" + body + "\n"
}

// startBaseToHead runs the runner over the PR whose base is baseSHA and whose
// head is headSHA, with the model answering from script, and returns the
// verdict, the model's calls, and Start's error.
func startBaseToHead(t *testing.T, repoDir, baseSHA, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, []llm.Request, error) {
	t.Helper()

	model := &fakeModel{script: script}
	runner := newRunner(model)
	runner.SetRemote(repoDir)

	req := testRequest(headSHA)
	req.BaseSHA = baseSHA
	req.ChangedFiles = changed
	started, err := runner.Start(t.Context(), req)
	if err != nil {
		return nil, model.calls, fmt.Errorf("start: %w", err)
	}
	result, ok := started.(review.Result)
	if !ok {
		t.Fatalf("Start() = %T, want review.Result", started)
	}
	return result.Verdict, model.calls, nil
}

// mustStartBaseToHead is startBaseToHead for runs that must succeed.
func mustStartBaseToHead(t *testing.T, repoDir, baseSHA, headSHA string, changed []review.ChangedFile, script ...func(llm.Request) (llm.Response, error)) (review.Verdict, []llm.Request) {
	t.Helper()

	verdict, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, changed, script...)
	if err != nil {
		t.Fatalf("Start() = %v, want nil error", err)
	}
	return verdict, calls
}

func mainGoChange() review.ChangedFile {
	return review.ChangedFile{Path: "main.go", Hunks: []review.LineRange{{Start: 1, End: 3}}, Patch: "@@ -1,2 +1,3 @@\n func main() {}\n"}
}

func TestStart_CandidateReplacedBySymlinkAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("remove docs/x.md: %v", err)
	}
	if err := os.Symlink("../main.go", filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("symlink docs/x.md: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseInternal {
		t.Fatalf("Start() error = %v, want *review.FailedError with CauseInternal", err)
	}
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
}

func TestStart_HeadGitattributesDoNotRewriteBaseDocs(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	commitDoc(t, repoDir, ".gitattributes", "docs/** working-tree-encoding=UTF-16LE-BOM\n")
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls := mustStartBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()}, triageResponse(false))
	if len(calls) != 1 {
		t.Fatalf("model saw %d calls, want 1 triage call for docs/x.md", len(calls))
	}
}

func TestStart_CandidateUnderASymlinkedDirectoryAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("remove docs/x.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "sub"), 0o700); err != nil {
		t.Fatalf("mkdir docs/sub: %v", err)
	}
	baseSHA := commitDoc(t, repoDir, "docs/sub/x.md", docWithCovers("\n  - main.go", "## Mid\nmid text\n"))

	// At head docs/sub is a symlink to a directory holding the same doc.
	if err := os.Rename(filepath.Join(repoDir, "docs", "sub"), filepath.Join(repoDir, "docs", "real")); err != nil {
		t.Fatalf("rename docs/sub: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(repoDir, "docs", "sub")); err != nil {
		t.Fatalf("symlink docs/sub: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseInternal {
		t.Fatalf("Start() error = %v, want *review.FailedError with CauseInternal", err)
	}
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
}

func TestStart_DocsReplacedBySymlinkedDirectoryAtHeadFailsWithoutModelCalls(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	if err := os.Rename(filepath.Join(repoDir, "docs"), filepath.Join(repoDir, "real")); err != nil {
		t.Fatalf("rename docs: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(repoDir, "docs")); err != nil {
		t.Fatalf("symlink docs: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, calls, err := startBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseInternal {
		t.Fatalf("Start() error = %v, want *review.FailedError with CauseInternal", err)
	}
	if len(calls) != 0 {
		t.Errorf("model saw %d calls, want 0", len(calls))
	}
}
