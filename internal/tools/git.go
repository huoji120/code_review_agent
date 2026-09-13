package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type gitInspectArgs struct {
	Action    string `json:"action"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	Ref       string `json:"ref"`
	Commit    string `json:"commit"`
	Path      string `json:"path"`
	LineStart int    `json:"line_start"`
	LineEnd   int    `json:"line_end"`
	Limit     int    `json:"limit"`
	Skip      int    `json:"skip"`
	Author    string `json:"author"`
	Since     string `json:"since"`
	Until     string `json:"until"`
	Query     string `json:"query"`
	Search    string `json:"search"`
	All       bool   `json:"all"`
	Follow    bool   `json:"follow"`
	Patch     bool   `json:"patch"`
	Context   int    `json:"context"`
	Staged    bool   `json:"staged"`
	Unstaged  bool   `json:"unstaged"`
}

type gitInspectResult struct {
	Action  string   `json:"action"`
	Command []string `json:"command"`
	Files   []string `json:"files,omitempty"`
	Output  string   `json:"output,omitempty"`
}

var gitRefRe = regexp.MustCompile(`^[A-Za-z0-9._/@{}^~+-]+$`)

func (r *Registry) GitPrompt() string {
	var b strings.Builder
	b.WriteString("# 当前 Git 状态\n\n")
	b.WriteString("- 工作区：")
	b.WriteString(filepath.ToSlash(r.workspace))
	b.WriteString("\n")
	if _, err := exec.LookPath("git"); err != nil {
		b.WriteString("- Git：不可用（未找到 git 可执行文件）。不要调用 `git_inspect`。")
		return b.String()
	}
	inside, err := r.runGitRaw("rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(inside) != "true" {
		b.WriteString("- Git：不可用（当前工作区不是 Git 仓库）。不要调用 `git_inspect`。")
		return b.String()
	}
	b.WriteString("- Git：可用。可以按需调用只读 `git_inspect`。\n")
	b.WriteString("- 历史：log 支持 ref 或 base/head、limit/skip 分页、author/since/until/query（字面消息）/search（-S）、all/follow；show 可读历史 path，log/show 可用 patch。branches/tags/tree/grep/rev_parse/merge_base 用于只读导航。大结果请先 read_tool_buffer 读取完整内容，或缩小 query/path；下一次非 buffer 工具调用会清除旧 buffer。\n")
	if root, err := r.runGitRaw("rev-parse", "--show-toplevel"); err == nil && strings.TrimSpace(root) != "" {
		b.WriteString("- 仓库根目录：")
		b.WriteString(filepath.ToSlash(strings.TrimSpace(root)))
		b.WriteString("\n")
	}
	status, err := r.runGitRaw("status", "--short", "--branch")
	if err != nil || strings.TrimSpace(status) == "" {
		b.WriteString("- 当前状态：clean")
		return b.String()
	}
	status, _ = trimGitOutput(status, 4000)
	b.WriteString("- 当前状态：\n")
	b.WriteString("```text\n")
	b.WriteString(status)
	b.WriteString("\n```")
	return b.String()
}

func (r *Registry) gitInspect(raw json.RawMessage) Result {
	if err := r.requireGitRepo(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	args, err := decodeArgs[gitInspectArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := args.validate(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	args.normalize()
	commandArgs, err := r.gitCommandArgs(args)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	output, err := r.runGit(commandArgs...)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	result := gitInspectResult{Action: args.Action, Command: append([]string{"git"}, commandArgs...), Output: output}
	if args.Action == "changed_files" {
		result.Files = parseGitChangedFiles(output, gitCommandReturnsNameOnly(commandArgs))
		result.Output = ""
	}
	return Result{OK: true, Data: result}
}

func (a *gitInspectArgs) normalize() {
	a.Action = strings.ToLower(strings.TrimSpace(a.Action))
	if a.Action == "" {
		a.Action = "status"
	}
	if a.Limit <= 0 {
		a.Limit = 50
	}
	if a.Limit > 200 {
		a.Limit = 200
	}
	if a.Context < 0 {
		a.Context = 0
	}
	if a.Ref == "" {
		a.Ref = a.Commit
	}
	if a.Ref == "" {
		a.Ref = "HEAD"
	}
	if a.Head == "" {
		a.Head = "HEAD"
	}
}

func (r *Registry) gitCommandArgs(args gitInspectArgs) ([]string, error) {
	path, err := r.gitPathArg(args.Path)
	if err != nil {
		return nil, err
	}
	switch args.Action {
	case "status":
		return []string{"status", "--short", "--branch", "--", gitWorkspacePath(path)}, nil
	case "changed_files":
		return r.gitChangedFilesArgs(args, gitWorkspacePath(path))
	case "diff":
		return r.gitDiffArgs(args, gitWorkspacePath(path))
	case "log":
		return r.gitLogArgs(args, path)
	case "show":
		return r.gitShowArgs(args, path)
	case "blame":
		return r.gitBlameArgs(args, path)
	case "branches":
		command := []string{"for-each-ref", "--format=%(refname:short)\t%(objectname)\t%(refname)", "refs/heads/"}
		if args.All {
			command = append(command, "refs/remotes/")
		}
		return command, nil
	case "tags":
		return []string{"for-each-ref", "--format=%(refname:short)\t%(objectname)\t%(refname)", "refs/tags/"}, nil
	case "tree", "grep":
		ref, err := r.gitCommitRef(args.Ref)
		if err != nil {
			return nil, err
		}
		if args.Action == "tree" {
			return []string{"ls-tree", "-r", "--full-name", ref, "--", gitWorkspacePath(path)}, nil
		}
		return []string{"grep", "--no-textconv", "-n", "-F", "-e", args.Query, ref, "--", gitWorkspacePath(path)}, nil
	case "rev_parse":
		if err := validateGitRef(args.Ref); err != nil {
			return nil, err
		}
		return []string{"rev-parse", "--verify", "--end-of-options", args.Ref + "^{commit}"}, nil
	case "merge_base":
		base, err := r.gitCommitRef(args.Base)
		if err != nil {
			return nil, err
		}
		head, err := r.gitCommitRef(args.Head)
		if err != nil {
			return nil, err
		}
		return []string{"merge-base", base, head}, nil
	default:
		return nil, fmt.Errorf("unsupported git action: %s", args.Action)
	}
}

func (r *Registry) gitChangedFilesArgs(args gitInspectArgs, path string) ([]string, error) {
	if args.Staged {
		command := []string{"diff", "--no-ext-diff", "--no-textconv", "--name-only", "--cached"}
		if path != "" {
			command = append(command, "--", path)
		}
		return command, nil
	}
	if args.Unstaged {
		command := []string{"diff", "--no-ext-diff", "--no-textconv", "--name-only"}
		if path != "" {
			command = append(command, "--", path)
		}
		return command, nil
	}
	if args.Base != "" {
		if err := validateGitRef(args.Base); err != nil {
			return nil, err
		}
		if err := validateGitRef(args.Head); err != nil {
			return nil, err
		}
		command := []string{"diff", "--no-ext-diff", "--no-textconv", "--name-only", args.Base + ".." + args.Head}
		if path != "" {
			command = append(command, "--", path)
		}
		return command, nil
	}
	command := []string{"status", "--porcelain", "-uall"}
	if path != "" {
		command = append(command, "--", path)
	}
	return command, nil
}

func (r *Registry) gitDiffArgs(args gitInspectArgs, path string) ([]string, error) {
	command := []string{"diff", "--no-ext-diff", "--no-textconv", "--unified=" + strconv.Itoa(args.Context)}
	if args.Staged {
		command = append(command, "--cached")
	} else if args.Base != "" {
		if err := validateGitRef(args.Base); err != nil {
			return nil, err
		}
		if err := validateGitRef(args.Head); err != nil {
			return nil, err
		}
		command = append(command, args.Base+".."+args.Head)
	} else if !args.Unstaged {
		command = append(command, "HEAD")
	}
	if path != "" {
		command = append(command, "--", path)
	}
	return command, nil
}

func (r *Registry) gitShowArgs(args gitInspectArgs, path string) ([]string, error) {
	ref, err := r.gitCommitRef(args.Ref)
	if err != nil {
		return nil, err
	}
	if path == "" {
		command := []string{"show", "--no-ext-diff", "--no-textconv", "--stat", "--oneline", "--decorate"}
		if args.Patch {
			command = append(command, "--patch", "--unified="+strconv.Itoa(args.Context))
		}
		return append(command, ref, "--", "."), nil
	}
	repoPath, err := r.gitRepoPath(path)
	if err != nil {
		return nil, err
	}
	return []string{"cat-file", "blob", ref + ":" + repoPath}, nil
}

func (r *Registry) gitBlameArgs(args gitInspectArgs, path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("path is required for blame")
	}
	ref, err := r.gitCommitRef(args.Ref)
	if err != nil {
		return nil, err
	}
	command := []string{"blame", "--no-textconv", "--date=short"}
	if args.LineStart > 0 {
		lineEnd := args.LineEnd
		if lineEnd < args.LineStart {
			lineEnd = args.LineStart
		}
		command = append(command, "-L", fmt.Sprintf("%d,%d", args.LineStart, lineEnd))
	}
	command = append(command, ref, "--", path)
	return command, nil
}

func (r *Registry) gitPathArg(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	if strings.ContainsRune(input, '\x00') {
		return "", fmt.Errorf("git path contains NUL")
	}
	abs, err := r.safePath(input)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.workspace, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func (r *Registry) gitRepoPath(path string) (string, error) {
	prefix, err := r.runGit("rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Clean(strings.TrimRight(filepath.ToSlash(prefix), "\r\n") + path)), nil
}

func (r *Registry) runGit(args ...string) (string, error) {
	if err := r.requireGitRepo(); err != nil {
		return "", err
	}
	return r.runGitRaw(args...)
}

func (r *Registry) runGitRaw(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git unavailable: executable not found")
	}
	commandArgs := append([]string{"--no-optional-locks", "--literal-pathspecs", "-c", "core.quotepath=false", "-c", "core.fsmonitor=false", "-c", "color.ui=false", "-c", "diff.submodule=short", "-c", "log.showSignature=false", "-c", "log.follow=false", "--no-pager"}, args...)
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Dir = r.workspace
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1", "GIT_GLOB_PATHSPECS=0", "GIT_NOGLOB_PATHSPECS=0", "GIT_ICASE_PATHSPECS=0")
	output, err := cmd.CombinedOutput()
	text := strings.TrimRight(string(output), "\r\n")
	if len(args) > 0 && args[0] == "cat-file" {
		text = string(output)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return text, fmt.Errorf("git command timed out")
	}
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 && len(args) > 0 && args[0] == "grep" && text == "" {
			return "", nil
		}
		if strings.TrimSpace(text) == "" {
			return text, err
		}
		return text, fmt.Errorf("%s", text)
	}
	return text, nil
}

func (r *Registry) requireGitRepo() error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git unavailable: executable not found")
	}
	inside, err := r.runGitRaw("rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("git unavailable: workspace is not a git repository")
	}
	if strings.TrimSpace(inside) != "true" {
		return fmt.Errorf("git unavailable: workspace is not inside a git work tree")
	}
	return nil
}

func validateGitRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return fmt.Errorf("git ref is required")
	}
	if strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\r\n") || !gitRefRe.MatchString(ref) {
		return fmt.Errorf("invalid git ref: %q", ref)
	}
	return nil
}

func gitCommandReturnsNameOnly(commandArgs []string) bool {
	for _, arg := range commandArgs {
		if arg == "--name-only" {
			return true
		}
	}
	return false
}

func parseGitChangedFiles(output string, nameOnly bool) []string {
	var files []string
	for _, rawLine := range strings.Split(strings.TrimRight(output, "\r\n"), "\n") {
		line := strings.TrimRight(rawLine, "\r")
		if line == "" {
			continue
		}
		if nameOnly {
			files = append(files, filepath.ToSlash(strings.TrimSpace(line)))
			continue
		}
		if len(line) <= 3 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if idx := strings.LastIndex(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		files = append(files, filepath.ToSlash(path))
	}
	return files
}

func trimGitOutput(output string, limit int) (string, bool) {
	if limit <= 0 || len(output) <= limit {
		return output, false
	}
	return output[:limit] + "\n...truncated...", true
}
