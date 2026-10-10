package llmrunner

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing/fstest"
	"time"

	"github.com/mrkizildag/pollux-agent/backend/internal/review/pipeline"
)

const maxGitOutputLen = 500

var fullSHA = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// clone is a fetched repository: its checked-out commit opened as root, and
// what git needs to read the other commits fetched with it.
type clone struct {
	root      *os.Root
	dir       string
	remoteURL string
	token     string
}

// cloneAt fetches checkout and alsoFetch from remoteURL at depth 1 into a new
// temp directory and checks checkout out detached, authenticating with token
// if it's non-empty. The alsoFetch commits are only fetched, never checked
// out. It returns the directory even on error once one was created, so the
// caller can always remove it.
func cloneAt(ctx context.Context, remoteURL, token, checkout string, alsoFetch ...string) (string, error) {
	shas := append([]string{checkout}, alsoFetch...)
	for _, sha := range shas {
		if !fullSHA.MatchString(sha) {
			return "", fmt.Errorf("clone %s: sha %q is not a full hex object id", remoteURL, sha)
		}
	}

	dir, err := os.MkdirTemp("", "pollux-agent-clone-")
	if err != nil {
		return "", fmt.Errorf("clone %s: create temp dir: %w", remoteURL, err)
	}

	steps := [][]string{
		{"init"},
		append([]string{"fetch", "--depth=1", "--no-tags", remoteURL}, shas...),
		{"checkout", "--detach", checkout},
	}
	for _, args := range steps {
		if _, err := runGit(ctx, dir, remoteURL, token, args...); err != nil {
			return dir, fmt.Errorf("clone %s at %s: %w", remoteURL, checkout, err)
		}
	}

	return dir, nil
}

// openClone clones sha of owner/repo, also fetching alsoFetch, and opens its
// root. cleanup closes the root and removes the clone; it is non-nil only when
// err is nil.
func (b *Backend) openClone(ctx context.Context, installationID int64, owner, repo, sha string, alsoFetch ...string) (*clone, func(), error) {
	token, err := b.token(ctx, installationID, repo)
	if err != nil {
		return nil, nil, fmt.Errorf("get installation token: %w: %w", pipeline.ErrWorkspace, err)
	}

	remoteURL := b.remote
	if remoteURL == "" {
		remoteURL = fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	}

	dir, err := cloneAt(ctx, remoteURL, token, sha, alsoFetch...)
	if err != nil {
		if dir != "" {
			_ = os.RemoveAll(dir) // best-effort cleanup of a temp dir; the backend has no logger
		}
		return nil, nil, fmt.Errorf("%w: %w", pipeline.ErrWorkspace, err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, fmt.Errorf("open clone root: %w", err)
	}
	return &clone{root: root, dir: dir, remoteURL: remoteURL, token: token}, func() {
		_ = root.Close()
		_ = os.RemoveAll(dir)
	}, nil
}

// docsAt reads docs/ at sha, which openClone fetched, straight from git
// objects and returns it as an in-memory fs.FS rooted at the repo
// root. Nothing is checked out, so the PR's .gitattributes can't rewrite the
// base docs, and the agent's root over the clone can't reach them. Only regular
// .md files of at most pipeline.MaxDocBytes are included.
func (c *clone) docsAt(ctx context.Context, baseSHA string) (fs.FS, error) {
	listing, err := runGit(ctx, c.dir, c.remoteURL, c.token, "ls-tree", "-r", "-z", "--long", baseSHA, "--", "docs")
	if err != nil {
		return nil, fmt.Errorf("list docs at %s: %w", baseSHA, err)
	}

	var paths, shas []string
	for _, entry := range strings.Split(listing, "\x00") {
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok || !strings.HasSuffix(path, ".md") {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) != 4 || (fields[0] != "100644" && fields[0] != "100755") || fields[1] != "blob" {
			continue
		}
		if size, err := strconv.Atoi(fields[3]); err != nil || size > pipeline.MaxDocBytes {
			continue
		}
		paths = append(paths, path)
		shas = append(shas, fields[2])
	}

	files := fstest.MapFS{}
	if len(shas) == 0 {
		return files, nil
	}

	out, err := runGitStdin(ctx, c.dir, c.remoteURL, c.token, strings.Join(shas, "\n")+"\n", "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read docs at %s: %w", baseSHA, err)
	}
	r := bufio.NewReader(strings.NewReader(out))
	for i, path := range paths {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("read docs at %s: header of %s: %w", baseSHA, path, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[0] != shas[i] || fields[1] != "blob" {
			return nil, fmt.Errorf("read docs at %s: unexpected cat-file header %q for %s", baseSHA, strings.TrimSpace(header), path)
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil || size < 0 || size > pipeline.MaxDocBytes {
			return nil, fmt.Errorf("read docs at %s: bad size in cat-file header %q for %s", baseSHA, strings.TrimSpace(header), path)
		}
		content := make([]byte, size+1) // the blob plus its trailing newline
		if _, err := io.ReadFull(r, content); err != nil {
			return nil, fmt.Errorf("read docs at %s: content of %s: %w", baseSHA, path, err)
		}
		files[path] = &fstest.MapFile{Data: content[:size], Mode: 0o444}
	}
	return files, nil
}

// isGitlink reports whether path is a submodule entry in the checked-out commit.
func (c *clone) isGitlink(ctx context.Context, path string) (bool, error) {
	out, err := runGit(ctx, c.dir, c.remoteURL, c.token, "ls-tree", "-z", "HEAD", "--", path)
	if err != nil {
		return false, fmt.Errorf("look up %s at head: %w", path, err)
	}
	return strings.HasPrefix(out, "160000 "), nil
}

// runGit runs git in dir and returns its stdout.
func runGit(ctx context.Context, dir, remoteURL, token string, args ...string) (string, error) {
	return runGitStdin(ctx, dir, remoteURL, token, "", args...)
}

// runGitStdin is runGit with stdin fed to git.
func runGitStdin(ctx context.Context, dir, remoteURL, token, stdin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // args are fixed git subcommands plus validated SHAs and the runner's remote, not request text
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = gitEnv(dir, remoteURL, token)
	// git fetch forks git-remote-http, which inherits the output pipe; killing
	// only git leaves it holding the pipe open, so kill the whole group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); err != nil && ctxErr != nil {
		return "", fmt.Errorf("git %v: %w", args, ctxErr)
	}
	if err != nil {
		return "", fmt.Errorf("git %v: %w: %s", args, err, oneLine(stderr.String(), maxGitOutputLen))
	}
	return string(out), nil
}

// gitEnv is the whole environment git runs in: it handles attacker-controlled
// repository content, so it gets none of the server's secrets, no system or
// user config, and only https and local-path remotes (plus http when the
// remote itself is http). Without a token the caller's
// GIT_CONFIG_COUNT/KEY_n/VALUE_n pass through so tests can redirect the remote
// with url.<path>.insteadOf.
func gitEnv(home, remoteURL, token string) []string {
	protocols := "https:file"
	if strings.HasPrefix(remoteURL, "http://") {
		protocols += ":http"
	}
	env := []string{
		"HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ALLOW_PROTOCOL=" + protocols,
	}
	if token != "" {
		env = append(env, gitAuthEnv(token)...)
	}
	for _, kv := range os.Environ() { //nolint:forbidigo // the one place the git subprocess's environment is allowed through, field by field
		switch {
		case strings.HasPrefix(kv, "PATH="):
			env = append(env, kv)
		case token == "" && (strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") || strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_")):
			env = append(env, kv)
		}
	}
	return env
}

// gitAuthEnv carries the clone's bearer token as an HTTP header through git's
// config-from-env mechanism, never in argv, a URL, or an on-disk config file.
func gitAuthEnv(token string) []string {
	if token == "" {
		return nil
	}
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + header,
	}
}
