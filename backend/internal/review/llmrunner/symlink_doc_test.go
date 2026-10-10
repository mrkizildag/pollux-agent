package llmrunner_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

// A candidate doc that the PR turns into a symlink to another regular file is
// refused, not read through the link, and the run fails.
func TestStart_CandidateDocThatIsASymlinkAtHeadFails(t *testing.T) {
	t.Parallel()

	repoDir, baseSHA := newGitRepo(t)
	commitDoc(t, repoDir, "docs/real.md", docWithCovers(" []", "real body."))
	if err := os.Remove(filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("remove docs/x.md: %v", err)
	}
	if err := os.Symlink("real.md", filepath.Join(repoDir, "docs", "x.md")); err != nil {
		t.Fatalf("symlink docs/x.md: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), "git", "add", "-A") //nolint:gosec // literal args
	cmd.Dir = repoDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	headSHA := commitDoc(t, repoDir, "main.go", mainGoChanged)

	_, _, err := startBaseToHead(t, repoDir, baseSHA, headSHA, []review.ChangedFile{mainGoChange()})
	var failed *review.FailedError
	if !errors.As(err, &failed) || failed.Cause != review.CauseInternal {
		t.Fatalf("Start() = %v, want a *review.FailedError with cause %q", err, review.CauseInternal)
	}
	if !strings.Contains(err.Error(), "docs/x.md") {
		t.Errorf("Start() error = %v, want it to name docs/x.md", err)
	}
}
