package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type gitHistoryFixture struct {
	root      string
	workspace string
	commits   []string
}

func historyGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=" + filepath.ToSlash(filepath.Join(root, ".no-hooks")), "-c", "commit.gpgSign=false", "-c", "core.autocrlf=false"}, args...)...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\r\n")
}

func historyWrite(t *testing.T, root, path, content string) {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func newGitHistoryFixture(t *testing.T) gitHistoryFixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable required for real history regression")
	}
	root := t.TempDir()
	// Isolate both fixture writes and Registry reads from user Git configuration.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, ".empty-gitconfig"))
	t.Setenv("GIT_CONFIG_COUNT", "0")
	historyGit(t, root, "init", "--quiet")
	historyGit(t, root, "symbolic-ref", "HEAD", "refs/heads/main")
	f := gitHistoryFixture{root: root, workspace: filepath.Join(root, "scope")}
	commit := func(author, date, message string) {
		t.Helper()
		t.Setenv("GIT_AUTHOR_NAME", author)
		t.Setenv("GIT_AUTHOR_EMAIL", strings.ToLower(author)+"@example.invalid")
		t.Setenv("GIT_COMMITTER_NAME", author)
		t.Setenv("GIT_COMMITTER_EMAIL", strings.ToLower(author)+"@example.invalid")
		t.Setenv("GIT_AUTHOR_DATE", date)
		t.Setenv("GIT_COMMITTER_DATE", date)
		historyGit(t, root, "add", "--all")
		historyGit(t, root, "commit", "--quiet", "-m", message)
		f.commits = append(f.commits, historyGit(t, root, "rev-parse", "HEAD"))
	}
	historyWrite(t, root, "outside.txt", "boundary-secret\n")
	historyWrite(t, root, "scope/old.txt", "stable line\nneedle.initial\n")
	historyWrite(t, root, "scope/deleted.txt", "deleted historical content\n")
	historyWrite(t, root, "scope/[x].txt", "literal file needle.[x]\n")
	historyWrite(t, root, "scope/x.txt", "decoy file needleZx\n")
	commit("Alice", "2024-01-01T12:00:00Z", "history-origin")
	historyGit(t, root, "mv", "scope/old.txt", "scope/renamed.txt")
	historyGit(t, root, "rm", "scope/deleted.txt")
	historyWrite(t, root, "scope/renamed.txt", "stable line\nneedle.initial\nintroduced-token\n")
	commit("Bob", "2024-01-02T12:00:00Z", "literal.[x] change")
	historyWrite(t, root, "scope/renamed.txt", "latest line\nneedle.initial\n")
	commit("Alice", "2024-01-03T12:00:00Z", "literalZx change")
	historyWrite(t, root, "scope/head.txt", "head-only content\n")
	commit("Bob", "2024-01-04T12:00:00Z", "history-head")
	historyGit(t, root, "branch", "historical", f.commits[0])
	historyGit(t, root, "tag", "release-history", f.commits[1])
	historyGit(t, root, "update-ref", "refs/remotes/origin/history-remote", f.commits[2])
	return f
}

func historyRegistry(t *testing.T, workspace string, budget int) *Registry {
	t.Helper()
	r, err := NewRegistry(workspace, budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func historyCall(t *testing.T, r *Registry, args map[string]interface{}) (bool, string) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Data  struct {
			Output string `json:"output"`
		} `json:"data"`
	}
	out := r.Call("git_inspect", raw)
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("invalid Git result: %v\n%s", err, out)
	}
	if !result.OK {
		return false, result.Error
	}
	return true, result.Data.Output
}

func historyOutput(t *testing.T, r *Registry, args map[string]interface{}) string {
	t.Helper()
	ok, output := historyCall(t, r, args)
	if !ok {
		t.Fatalf("git_inspect %v failed: %s", args, output)
	}
	return output
}

func TestGitHistoryDeletedAndRenamedPathsFromSubdirectory(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	for path, want := range map[string]string{"old.txt": "stable line\nneedle.initial", "deleted.txt": "deleted historical content"} {
		out := historyOutput(t, r, map[string]interface{}{"action": "show", "ref": f.commits[0], "path": path})
		if strings.TrimSpace(out) != want {
			t.Fatalf("historical %s: %q, want %q", path, out, want)
		}
	}
	out := historyOutput(t, r, map[string]interface{}{"action": "log", "ref": f.commits[1], "path": "renamed.txt", "follow": true})
	if !strings.Contains(out, "history-origin") || !strings.Contains(out, "literal.[x] change") || strings.Contains(out, "literalZx change") {
		t.Fatalf("rename history did not follow historical ref: %s", out)
	}
	tree := historyOutput(t, r, map[string]interface{}{"action": "tree", "ref": f.commits[0]})
	if !strings.Contains(tree, "deleted.txt") || !strings.Contains(tree, "old.txt") || strings.Contains(tree, "outside.txt") || strings.Contains(tree, "head.txt") {
		t.Fatalf("historical tree leaked scope or used current files: %s", tree)
	}
	matches := historyOutput(t, r, map[string]interface{}{"action": "grep", "ref": f.commits[0], "query": "deleted historical"})
	if !strings.Contains(matches, "deleted.txt:1:deleted historical content") {
		t.Fatalf("historical grep missed deleted file/line: %s", matches)
	}
	if out := historyOutput(t, r, map[string]interface{}{"action": "grep", "query": "boundary-secret"}); out != "" {
		t.Fatalf("default historical search escaped subdirectory: %s", out)
	}
}

func TestGitHistoryLogFiltersRangesAndPagination(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	cases := []struct {
		name   string
		args   map[string]interface{}
		want   []string
		absent []string
	}{
		{"ref", map[string]interface{}{"ref": "historical"}, []string{"history-origin"}, []string{"history-head", "literal.[x] change"}},
		{"range", map[string]interface{}{"base": f.commits[0], "head": f.commits[2]}, []string{"literal.[x] change", "literalZx change"}, []string{"history-origin", "history-head"}},
		{"literal-message", map[string]interface{}{"query": "literal.[x]"}, []string{"literal.[x] change"}, []string{"literalZx change", "history-head", "history-origin"}},
		{"author", map[string]interface{}{"author": "Alice"}, []string{"history-origin", "literalZx change"}, []string{"literal.[x] change", "history-head"}},
		{"date-window", map[string]interface{}{"since": "2024-01-02T00:00:00Z", "until": "2024-01-02T23:59:59Z"}, []string{"literal.[x] change"}, []string{"history-origin", "literalZx change", "history-head"}},
		{"string-change", map[string]interface{}{"search": "introduced-token"}, []string{"literal.[x] change", "literalZx change"}, []string{"history-origin", "history-head"}},
		{"first-page", map[string]interface{}{"limit": 1}, []string{"history-head"}, []string{"literalZx change", "literal.[x] change", "history-origin"}},
		{"second-page", map[string]interface{}{"limit": 1, "skip": 1}, []string{"literalZx change"}, []string{"history-head", "literal.[x] change", "history-origin"}},
		{"filtered-page", map[string]interface{}{"author": "Bob", "limit": 1, "skip": 1}, []string{"literal.[x] change"}, []string{"history-head", "literalZx change", "history-origin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["action"] = "log"
			out := historyOutput(t, r, tc.args)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in %s", want, out)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(out, absent) {
					t.Errorf("unexpected %q in %s", absent, out)
				}
			}
		})
	}
}

func TestGitHistoryPatchAndRefSpecificBlame(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	for _, action := range []string{"show", "log"} {
		out := historyOutput(t, r, map[string]interface{}{"action": action, "ref": f.commits[2], "patch": true, "context": 1})
		if !strings.Contains(out, "-stable line") || !strings.Contains(out, "+latest line") || !strings.Contains(out, " needle.initial") {
			t.Fatalf("%s missing requested historical patch/context: %s", action, out)
		}
	}
	out := historyOutput(t, r, map[string]interface{}{"action": "show", "ref": f.commits[2]})
	if strings.Contains(out, "@@") || strings.Contains(out, "+latest line") {
		t.Fatalf("summary unexpectedly includes patch: %s", out)
	}
	out = historyOutput(t, r, map[string]interface{}{"action": "blame", "ref": f.commits[1], "path": "renamed.txt", "line_start": 1, "line_end": 1})
	if !strings.Contains(out, "stable line") || !strings.Contains(out, "Alice") || strings.Contains(out, "latest line") || strings.Contains(out, "needle.initial") {
		t.Fatalf("blame ignored historical ref or line boundary: %s", out)
	}
}

func TestGitHistoryLiteralPathsAndUnsafeInputs(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	out := historyOutput(t, r, map[string]interface{}{"action": "grep", "query": "file", "path": "[x].txt"})
	if !strings.Contains(out, "literal file") || strings.Contains(out, "decoy file") {
		t.Fatalf("Git expanded literal bracket path: %s", out)
	}
	out = historyOutput(t, r, map[string]interface{}{"action": "grep", "query": "needle.[x]"})
	if !strings.Contains(out, "literal file") || strings.Contains(out, "decoy file") {
		t.Fatalf("Git treated content query as regex: %s", out)
	}
	out = historyOutput(t, r, map[string]interface{}{"action": "grep", "query": "boundary-secret", "path": ":(top)outside.txt"})
	if out != "" {
		t.Fatalf("Git pathspec magic escaped workspace: %s", out)
	}
	marker := filepath.Join(f.root, "injected-marker")
	for _, action := range []string{"log", "show", "blame", "tree", "grep", "rev_parse"} {
		for _, ref := range []string{"--help", "--output=" + marker, "HEAD;touch " + marker} {
			args := map[string]interface{}{"action": action, "ref": ref}
			if action == "blame" {
				args["path"] = "renamed.txt"
			}
			if action == "grep" {
				args["query"] = "needle"
			}
			if ok, output := historyCall(t, r, args); ok {
				t.Errorf("%s accepted unsafe ref %q: %s", action, ref, output)
			}
		}
	}
	for _, path := range []string{"../outside.txt", filepath.Join(f.root, "outside.txt")} {
		if ok, out := historyCall(t, r, map[string]interface{}{"action": "show", "path": path}); ok {
			t.Errorf("accepted outside path %q: %s", path, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("unsafe ref created marker or failed stat: %v", err)
	}
	if out := historyGit(t, f.root, "status", "--porcelain"); out != "" {
		t.Fatalf("read-only inspection changed repository: %s", out)
	}
}

func TestGitHistoryReferenceDiscoveryAndOptionErrors(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	out := historyOutput(t, r, map[string]interface{}{"action": "branches"})
	if !strings.Contains(out, "historical") || !strings.Contains(out, f.commits[0]) || strings.Contains(out, "history-remote") {
		t.Fatalf("local branch discovery incorrect: %s", out)
	}
	out = historyOutput(t, r, map[string]interface{}{"action": "branches", "all": true})
	if !strings.Contains(out, "history-remote") || !strings.Contains(out, f.commits[2]) {
		t.Fatalf("remote branch discovery incorrect: %s", out)
	}
	out = historyOutput(t, r, map[string]interface{}{"action": "tags"})
	if !strings.Contains(out, "release-history") || !strings.Contains(out, f.commits[1]) {
		t.Fatalf("tag discovery incorrect: %s", out)
	}
	for _, args := range []map[string]interface{}{
		{"action": "rev_parse", "ref": "release-history"},
		{"action": "merge_base", "base": "release-history"},
	} {
		if out := historyOutput(t, r, args); strings.TrimSpace(out) != f.commits[1] {
			t.Errorf("incorrect commit identity for %v: %s", args, out)
		}
	}
	for _, args := range []map[string]interface{}{
		{"action": "log", "skip": -1},
		{"action": "log", "follow": true},
		{"action": "log", "follow": true, "path": "renamed.txt", "all": true},
		{"action": "log", "ref": "HEAD", "base": f.commits[0]},
		{"action": "show", "path": "renamed.txt", "patch": true},
		{"action": "grep"},
		{"action": "merge_base"},
		{"action": "tree", "skip": 1},
	} {
		if ok, out := historyCall(t, r, args); ok {
			t.Errorf("incompatible or missing option accepted: %v: %s", args, out)
		}
	}
}

func TestGitHistoryLargeFileBufferRecoversTail(t *testing.T) {
	f := newGitHistoryFixture(t)
	content := strings.Repeat("historical payload line\n", 3000) + "unique-historical-tail\n"
	historyWrite(t, f.root, "scope/large.txt", content)
	historyGit(t, f.root, "add", "scope/large.txt")
	historyGit(t, f.root, "commit", "--quiet", "-m", "large historical blob")
	r := historyRegistry(t, f.workspace, 1024)
	raw := json.RawMessage(`{"action":"show","path":"large.txt"}`)
	bounded, full := r.CallWithFullResult(context.Background(), "git_inspect", raw)
	var page bufferPage
	if err := json.Unmarshal([]byte(bounded), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated || page.BufferID == "" {
		t.Fatalf("missing oversized history buffer: %s", bounded)
	}
	id := page.BufferID
	recovered := page.Preview
	for !page.EOF {
		offset := page.NextOffset
		args, err := json.Marshal(readBufferArgs{BufferID: id, Offset: offset, Limit: 700})
		if err != nil {
			t.Fatal(err)
		}
		next := r.Call("read_tool_buffer", args)
		page = bufferPage{}
		if err := json.Unmarshal([]byte(next), &page); err != nil {
			t.Fatal(err)
		}
		if !page.OK || page.NextOffset <= offset {
			t.Fatalf("buffer did not advance at %d: %s", offset, next)
		}
		recovered += page.Content
	}
	if recovered != full {
		t.Fatal("buffer pagination changed full historical output")
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			Output string `json:"output"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(recovered), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || strings.TrimRight(result.Data.Output, "\r\n") != strings.TrimRight(content, "\r\n") {
		t.Fatalf("historical file lost content before buffering: recovered %d bytes, expected %d", len(result.Data.Output), len(content))
	}
}

func TestGitHistoryAllRefsIncludesUnmergedBranch(t *testing.T) {
	f := newGitHistoryFixture(t)
	historyGit(t, f.root, "checkout", "--quiet", "-b", "unmerged-history", f.commits[0])
	historyWrite(t, f.root, "scope/side.txt", "side branch evidence\n")
	historyGit(t, f.root, "add", "scope/side.txt")
	historyGit(t, f.root, "commit", "--quiet", "-m", "history-unmerged-only")
	historyGit(t, f.root, "checkout", "--quiet", "main")
	r := historyRegistry(t, f.workspace, 128000)
	if out := historyOutput(t, r, map[string]interface{}{"action": "log"}); strings.Contains(out, "history-unmerged-only") {
		t.Fatalf("default log crossed into unmerged branch: %s", out)
	}
	if out := historyOutput(t, r, map[string]interface{}{"action": "log", "all": true}); !strings.Contains(out, "history-unmerged-only") {
		t.Fatalf("all-ref log omitted unmerged branch: %s", out)
	}
}

func TestGitHistoryFollowRejectsRenameAcrossWorkspace(t *testing.T) {
	f := newGitHistoryFixture(t)
	historyGit(t, f.root, "mv", "outside.txt", "scope/inbound.txt")
	historyGit(t, f.root, "commit", "--quiet", "-m", "history-cross-boundary")
	r := historyRegistry(t, f.workspace, 128000)
	args := map[string]interface{}{"action": "log", "path": "inbound.txt", "follow": true, "patch": true}
	if ok, out := historyCall(t, r, args); ok || strings.Contains(out, "boundary-secret") {
		t.Fatalf("cross-boundary follow must reject without exposing outside history: ok=%v %s", ok, out)
	}
	rootRegistry := historyRegistry(t, f.root, 128000)
	args["path"] = "scope/inbound.txt"
	out := historyOutput(t, rootRegistry, args)
	if !strings.Contains(out, "history-origin") || !strings.Contains(out, "boundary-secret") {
		t.Fatalf("repository-root follow failed to recover authorized history: %s", out)
	}
}

func TestGitHistoryPreservesExistingDiffOptionPrecedence(t *testing.T) {
	f := newGitHistoryFixture(t)
	r := historyRegistry(t, f.workspace, 128000)
	historyWrite(t, f.root, "scope/staged.txt", "STAGED_ONLY\n")
	historyGit(t, f.root, "add", "--", "scope/staged.txt")
	historyWrite(t, f.root, "scope/head.txt", "UNSTAGED_ONLY\n")
	staged := historyOutput(t, r, map[string]interface{}{"action": "diff", "staged": true, "unstaged": true, "base": f.commits[0]})
	if !strings.Contains(staged, "STAGED_ONLY") || strings.Contains(staged, "UNSTAGED_ONLY") || strings.Contains(staged, "deleted historical content") {
		t.Fatal("staged diff no longer takes precedence over unstaged/base")
	}
	historical := historyOutput(t, r, map[string]interface{}{"action": "diff", "unstaged": true, "base": f.commits[0]})
	if !strings.Contains(historical, "deleted historical content") || strings.Contains(historical, "UNSTAGED_ONLY") {
		t.Fatal("diff base no longer takes precedence over unstaged")
	}
	for _, staged := range []bool{true, false} {
		args, _ := json.Marshal(map[string]interface{}{"action": "changed_files", "staged": staged, "unstaged": true, "base": f.commits[0]})
		var result struct {
			OK   bool `json:"ok"`
			Data struct {
				Files []string `json:"files"`
			} `json:"data"`
		}
		out := r.Call("git_inspect", args)
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		want := "head.txt"
		if staged {
			want = "staged.txt"
		}
		if !result.OK || len(result.Data.Files) != 1 || filepath.Base(result.Data.Files[0]) != want {
			t.Fatalf("changed_files precedence: %s", out)
		}
	}
}
