package llmrunner_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
	"github.com/mrkizildag/pollux-agent/backend/internal/review"
)

func TestStart_NewDocAtAnOversizedFileIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	big := "---\ntitle: Other\n---\n" + strings.Repeat("x", 2<<20)
	if err := os.WriteFile(filepath.Join(repoDir, "docs", "other.md"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	headSHA := commitDoc(t, repoDir, "docs/keep.txt", "keep\n")

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(newDocProposal("other.go")), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocAtADirectoryPathIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "other.md"), 0o700); err != nil {
		t.Fatalf("mkdir docs/other.md: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/other.md/keep.txt", "keep\n")

	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(newDocProposal("other.go")), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocUnderASymlinkedDirectoryIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(repoDir, "docs", "link")); err != nil {
		t.Fatalf("symlink docs/link: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/keep.txt", "keep\n")

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/link/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocUnderAnInCloneSymlinkIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	if err := os.MkdirAll(filepath.Join(repoDir, "docs", "real"), 0o700); err != nil {
		t.Fatalf("mkdir docs/real: %v", err)
	}
	if err := os.Symlink("real", filepath.Join(repoDir, "docs", "link")); err != nil {
		t.Fatalf("symlink docs/link: %v", err)
	}
	headSHA := commitDoc(t, repoDir, "docs/real/keep.txt", "keep\n")

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/link/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_HeadReadErrorFailsTheRun(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	proposal := newDocProposal("other.go")
	// A path component over the file system's name limit makes lstat fail with
	// an error that is neither "missing" nor fixable by the model.
	proposal["doc_path"] = "docs/" + strings.Repeat("a", 300) + ".md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}

	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		triageResponse(true), newDocResponse(true), submitResponse(proposal),
	}}
	runner := newRunner(model)
	runner.SetRemote(repoDir)
	req := testRequest(headSHA)
	req.ChangedFiles = changed

	if started, err := runner.Start(t.Context(), req); err == nil {
		t.Fatalf("Start() = %#v, nil; want the run to fail on the head read", started)
	}
	if len(model.calls) != 3 {
		t.Errorf("model calls = %d, want 3 (no retry after the head read failed)", len(model.calls))
	}
}

func TestStart_NewDocUnderAnExistingFileIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, headSHA := newGitRepo(t)
	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/x.md/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}

func TestStart_NewDocInsideASubmoduleIsReturnedToModel(t *testing.T) {
	t.Parallel()

	repoDir, _ := newGitRepo(t)
	// A gitlink entry: the clone checks it out as an empty directory. Git
	// accepts a gitlink to a commit that is not in the repository.
	const gitlinkSHA = "0123456789abcdef0123456789abcdef01234567"
	runInRepo := func(cmd *exec.Cmd) string {
		t.Helper()
		cmd.Dir = repoDir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v: %s", cmd.Args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runInRepo(exec.CommandContext(t.Context(), "git", "update-index", "--add", "--cacheinfo", "160000,"+gitlinkSHA+",docs/sub"))
	runInRepo(exec.CommandContext(t.Context(), "git", "commit", "-q", "-m", "add submodule"))
	headSHA := runInRepo(exec.CommandContext(t.Context(), "git", "rev-parse", "HEAD"))

	proposal := newDocProposal("other.go")
	proposal["doc_path"] = "docs/sub/new.md"
	changed := []review.ChangedFile{mainGoChange(), otherGoChange()}
	_, model := startOnRepo(t, repoDir, headSHA, changed,
		triageResponse(true), newDocResponse(true), submitResponse(proposal), submitResponse(), verifyResponse(true))
	if got := returnedToModel(t, model); !strings.Contains(got, "proposal 0:") || !strings.Contains(got, "already exists") {
		t.Errorf("tool error = %q, want proposal 0 reported as already existing", got)
	}
}
