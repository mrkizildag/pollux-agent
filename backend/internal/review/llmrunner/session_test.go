package llmrunner_test

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/llmrunner"
	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

func openSession(t *testing.T, repoDir string, ck pipeline.Checkout) (pipeline.Session, error) {
	t.Helper()

	backend := llmrunner.NewBackend(&fakeModel{}, noToken, "draft")
	backend.SetRemote(repoDir)
	ck.Owner, ck.Repo = "o", "r"
	session, err := backend.Open(t.Context(), ck)
	if err != nil {
		return nil, fmt.Errorf("open session: %w", err)
	}
	return session, nil
}

func TestOpen_RejectsBaseThatIsNotAFullObjectID(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)

	_, err := openSession(t, repoDir, pipeline.Checkout{Head: headSHA, Base: "--upload-pack=x"})
	if !errors.Is(err, pipeline.ErrWorkspace) {
		t.Fatalf("Open(base \"--upload-pack=x\") = %v, want errors.Is pipeline.ErrWorkspace", err)
	}
}

func TestBaseDocs_WithoutABaseFailsAsAWorkspaceError(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	s, err := openSession(t, repoDir, pipeline.Checkout{Head: headSHA})
	if err != nil {
		t.Fatalf("Open() = %v, want nil error", err)
	}
	defer s.Close()

	if _, err := s.BaseDocs(t.Context()); !errors.Is(err, pipeline.ErrWorkspace) {
		t.Fatalf("BaseDocs() = %v, want errors.Is pipeline.ErrWorkspace", err)
	}
}

// A path with git pathspec magic names a literal path, so a submodule checked
// out at ":(top)sub" is found at that path and not at "sub".
func TestStat_GitlinkAtAPathspecMagicPathIsOther(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	var gitlinkSHA string
	for _, args := range [][]string{
		{"update-index", "--add", "--cacheinfo", "160000," + headSHA + ",:(top)sub"},
		{"commit", "-q", "-m", "submodule"},
		{"rev-parse", "HEAD"},
	} {
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // headSHA is a commit id from the fixture repo
		cmd.Dir = repoDir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		gitlinkSHA = strings.TrimSpace(string(out))
	}

	s, err := openSession(t, repoDir, pipeline.Checkout{Head: gitlinkSHA})
	if err != nil {
		t.Fatalf("Open() = %v, want nil error", err)
	}
	defer s.Close()

	kind, err := s.Stat(t.Context(), ":(top)sub")
	if err != nil {
		t.Fatalf("Stat(:(top)sub) = %v, want nil error", err)
	}
	if kind != pipeline.Other {
		t.Errorf("Stat(:(top)sub) = %v, want pipeline.Other", kind)
	}
}
