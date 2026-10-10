package agent_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrkizildag/pollux-agent/backend/internal/agent"
	"github.com/mrkizildag/pollux-agent/backend/internal/llm"
)

const secret = "TOP-SECRET-OUTSIDE"

// runTool executes one tool call through Run and returns the tool result the
// model would see.
func runTool(t *testing.T, root *os.Root, name, args string) llm.ToolResult {
	t.Helper()

	var got llm.ToolResult
	model := &fakeModel{script: []func(llm.Request) (llm.Response, error){
		func(llm.Request) (llm.Response, error) {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: name, Args: json.RawMessage(args)}}}, nil
		},
		func(req llm.Request) (llm.Response, error) {
			last := req.Messages[len(req.Messages)-1]
			if len(last.ToolResults) != 1 {
				t.Fatalf("last message = %+v, want one tool result", last)
			}
			got = last.ToolResults[0]
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "2", Name: "submit", Args: json.RawMessage(`{}`)}}}, nil
		},
	}}
	task := agent.Task{
		Model: "m", Prompt: "go", Root: root, Finish: finishTool(),
		Accept:   func(json.RawMessage) error { return nil },
		MaxSteps: 5,
	}
	if _, _, err := agent.Run(t.Context(), model, task, &capCharger{max: 1_000_000}); err != nil {
		t.Fatalf("Run() = %v, want nil error", err)
	}
	return got
}

// escapeRoot returns a root holding a symlink to a secret file and a symlink
// to a directory containing it, both outside the root.
func escapeRoot(t *testing.T) *os.Root {
	t.Helper()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("fine"), 0o600); err != nil {
		t.Fatalf("write ok.txt: %v", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestTools_RejectEscapes(t *testing.T) {
	t.Parallel()

	root := escapeRoot(t)
	abs := filepath.Join(t.TempDir(), "x")

	tests := []struct{ name, tool, args string }{
		{"read parent", "read_file", `{"path":"../x"}`},
		{"read absolute", "read_file", `{"path":"` + abs + `"}`},
		{"read symlink", "read_file", `{"path":"link.txt"}`},
		{"read through symlink dir", "read_file", `{"path":"linkdir/secret.txt"}`},
		{"grep parent", "grep", `{"pattern":".","path":"../x"}`},
		{"grep absolute", "grep", `{"pattern":".","path":"` + abs + `"}`},
		{"grep symlink file", "grep", `{"pattern":".","path":"link.txt"}`},
		{"grep symlink dir", "grep", `{"pattern":".","path":"linkdir"}`},
		{"list parent", "list_dir", `{"path":".."}`},
		{"list absolute", "list_dir", `{"path":"` + filepath.Dir(abs) + `"}`},
		{"list symlink dir", "list_dir", `{"path":"linkdir"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := runTool(t, root, tc.tool, tc.args)
			if !res.IsError {
				t.Errorf("%s %s = %q, want an IsError result", tc.tool, tc.args, res.Content)
			}
			if strings.Contains(res.Content, secret) {
				t.Errorf("%s %s leaked outside content: %q", tc.tool, tc.args, res.Content)
			}
		})
	}
}

func TestGrep_RootWalkDoesNotFollowSymlinks(t *testing.T) {
	t.Parallel()

	res := runTool(t, escapeRoot(t), "grep", `{"pattern":"."}`)
	if res.IsError {
		t.Fatalf("grep = %q, want success", res.Content)
	}
	if strings.Contains(res.Content, secret) {
		t.Errorf("grep leaked outside content: %q", res.Content)
	}
	if !strings.Contains(res.Content, "ok.txt:1: fine") {
		t.Errorf("grep = %q, want ok.txt:1: fine", res.Content)
	}
}

func TestTools_RefuseGit(t *testing.T) {
	t.Parallel()

	root := testRoot(t)
	for _, tc := range []struct{ tool, args string }{
		{"read_file", `{"path":".git/config"}`},
		{"grep", `{"pattern":"secret","path":".git"}`},
		{"list_dir", `{"path":".git"}`},
	} {
		if res := runTool(t, root, tc.tool, tc.args); !res.IsError {
			t.Errorf("%s %s = %q, want an IsError result", tc.tool, tc.args, res.Content)
		}
	}
}

func TestGrep_SkipsGitAndBinary(t *testing.T) {
	t.Parallel()

	root := testRoot(t)
	if err := root.WriteFile("bin.dat", []byte("secret\x00secret"), 0o600); err != nil {
		t.Fatalf("write bin.dat: %v", err)
	}
	if err := root.WriteFile("a.txt", []byte("one\nsecret two\n"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}

	res := runTool(t, root, "grep", `{"pattern":"secret"}`)
	if res.IsError {
		t.Fatalf("grep = %q, want success", res.Content)
	}
	if res.Content != "a.txt:2: secret two\n" {
		t.Errorf("grep = %q, want only a.txt:2", res.Content)
	}
}

func TestGrep_ReportsUnreadableFiles(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root reads mode-0 files")
	}
	root := testRoot(t)
	if err := root.WriteFile("a.txt", []byte("hit\n"), 0o600); err != nil {
		t.Fatalf("write a.txt: %v", err)
	}
	if err := root.WriteFile("locked.txt", []byte("hit\n"), 0o000); err != nil {
		t.Fatalf("write locked.txt: %v", err)
	}

	res := runTool(t, root, "grep", `{"pattern":"hit"}`)
	if want := "a.txt:1: hit\n(skipped 1 unreadable files)\n"; res.Content != want {
		t.Errorf("grep = %q, want %q", res.Content, want)
	}
}

func TestGrep_CapsMatches(t *testing.T) {
	t.Parallel()

	root := testRoot(t)
	if err := root.WriteFile("many.txt", []byte(strings.Repeat("hit\n", 500)), 0o600); err != nil {
		t.Fatalf("write many.txt: %v", err)
	}

	res := runTool(t, root, "grep", `{"pattern":"hit"}`)
	if got := strings.Count(res.Content, "many.txt:"); got != 200 {
		t.Errorf("grep matches = %d, want 200", got)
	}
	if !strings.Contains(res.Content, "[truncated") {
		t.Errorf("grep output has no truncation note: ...%q", res.Content[max(0, len(res.Content)-80):])
	}
}

func TestReadFile_CapsSize(t *testing.T) {
	t.Parallel()

	root := testRoot(t)
	if err := root.WriteFile("big.txt", []byte(strings.Repeat("x", 100<<10)), 0o600); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}

	res := runTool(t, root, "read_file", `{"path":"big.txt"}`)
	if res.IsError {
		t.Fatalf("read_file = error %q", res.Content)
	}
	if len(res.Content) > 65<<10 {
		t.Errorf("read_file returned %d bytes, want about 64 KB", len(res.Content))
	}
	if !strings.Contains(res.Content, "[truncated") {
		t.Errorf("read_file output has no truncation note")
	}
}

func TestListDir_MarksDirsAndSkipsGit(t *testing.T) {
	t.Parallel()

	root := testRoot(t)
	if err := root.Mkdir("sub", 0o700); err != nil {
		t.Fatalf("mkdir sub: %v", err)
	}

	res := runTool(t, root, "list_dir", `{"path":"."}`)
	if res.IsError {
		t.Fatalf("list_dir = error %q", res.Content)
	}
	if res.Content != "doc.md\nsub/\n" {
		t.Errorf("list_dir = %q, want %q", res.Content, "doc.md\nsub/\n")
	}
}
