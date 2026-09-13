package tools

import (
	"fmt"
	"strconv"
	"strings"
)

// Validate explicit options before defaults hide whether an option was supplied.
func (a gitInspectArgs) validate() error {
	action := strings.ToLower(strings.TrimSpace(a.Action))
	if action == "" {
		action = "status"
	}
	allows := func(field string, supplied bool, actions ...string) error {
		if !supplied {
			return nil
		}
		for _, allowed := range actions {
			if action == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s is not supported for git action %s", field, action)
	}
	checks := []struct {
		field    string
		supplied bool
		actions  []string
	}{
		{"ref/commit", a.Ref != "" || a.Commit != "", []string{"log", "show", "blame", "tree", "grep", "rev_parse"}},
		{"base/head", a.Base != "" || a.Head != "", []string{"log", "diff", "changed_files", "merge_base"}},
		{"path", a.Path != "", []string{"status", "changed_files", "diff", "log", "show", "blame", "tree", "grep"}},
		{"limit/skip", a.Limit != 0 || a.Skip != 0, []string{"log"}},
		{"author/since/until/search", a.Author != "" || a.Since != "" || a.Until != "" || a.Search != "", []string{"log"}},
		{"query", a.Query != "", []string{"log", "grep"}},
		{"all", a.All, []string{"log", "branches"}},
		{"follow", a.Follow, []string{"log"}},
		{"patch", a.Patch, []string{"log", "show"}},
		{"context", a.Context != 0, []string{"diff", "log", "show"}},
		{"staged/unstaged", a.Staged || a.Unstaged, []string{"diff", "changed_files"}},
		{"line_start/line_end", a.LineStart != 0 || a.LineEnd != 0, []string{"blame"}},
	}
	for _, check := range checks {
		if err := allows(check.field, check.supplied, check.actions...); err != nil {
			return err
		}
	}
	if a.Skip < 0 {
		return fmt.Errorf("skip must be nonnegative")
	}
	if a.Context != 0 && (action == "log" || action == "show") && !a.Patch {
		return fmt.Errorf("context requires patch for %s", action)
	}
	if a.Ref != "" && a.Commit != "" && a.Ref != a.Commit {
		return fmt.Errorf("ref and commit must identify the same revision when both are supplied")
	}
	if action == "log" && a.Head != "" && a.Base == "" {
		return fmt.Errorf("head requires base")
	}
	if action == "log" {
		if a.Base != "" && (a.Ref != "" || a.Commit != "") {
			return fmt.Errorf("log accepts ref/commit or base/head, not both")
		}
		if a.All && (a.Follow || a.Ref != "" || a.Commit != "" || a.Base != "" || a.Head != "") {
			return fmt.Errorf("log all cannot be combined with follow or explicit revisions")
		}
		if a.Follow && (a.Path == "" || a.Path == ".") {
			return fmt.Errorf("follow requires one file path")
		}
	}
	if action == "show" && a.Patch && a.Path != "" {
		return fmt.Errorf("show with path reads a historical file; patch requires no path")
	}
	if action == "grep" && a.Query == "" {
		return fmt.Errorf("query is required for grep")
	}
	if action == "merge_base" && a.Base == "" {
		return fmt.Errorf("base is required for merge_base")
	}
	if a.LineStart < 0 || a.LineEnd < 0 || a.LineEnd > 0 && a.LineStart == 0 {
		return fmt.Errorf("blame lines require line_start > 0 and nonnegative line_end")
	}
	for _, value := range []string{a.Author, a.Since, a.Until, a.Query, a.Search} {
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("git query fields must not contain NUL")
		}
	}
	return nil
}

func gitWorkspacePath(path string) string {
	if path == "" {
		return "."
	}
	return path
}

func (r *Registry) gitCommitRef(ref string) (string, error) {
	if err := validateGitRef(ref); err != nil {
		return "", err
	}
	return r.runGit("rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func (r *Registry) gitLogArgs(args gitInspectArgs, path string) ([]string, error) {
	command := []string{"log", "--oneline", "--decorate", "--no-ext-diff", "--no-textconv", "--fixed-strings", "-n", strconv.Itoa(args.Limit), "--skip=" + strconv.Itoa(args.Skip)}
	if args.Author != "" {
		command = append(command, "--author="+args.Author)
	}
	if args.Since != "" {
		command = append(command, "--since="+args.Since)
	}
	if args.Until != "" {
		command = append(command, "--until="+args.Until)
	}
	if args.Query != "" {
		command = append(command, "--grep="+args.Query)
	}
	if args.Search != "" {
		command = append(command, "-S"+args.Search)
	}
	if args.Patch {
		command = append(command, "--patch", "--unified="+strconv.Itoa(args.Context))
	}
	var revision string
	if args.All {
		command = append(command, "--all")
	} else {
		ref := args.Ref
		if args.Base != "" {
			ref = args.Head
		}
		head, err := r.gitCommitRef(ref)
		if err != nil {
			return nil, err
		}
		revision = head
		if args.Base != "" {
			base, err := r.gitCommitRef(args.Base)
			if err != nil {
				return nil, err
			}
			revision = base + ".." + head
		}
	}
	if args.Follow {
		if path == "" || path == "." {
			return nil, fmt.Errorf("follow requires one file path")
		}
		if err := r.gitFollowBoundary(revision, path); err != nil {
			return nil, err
		}
		command = append(command, "--follow", "--find-renames")
	}
	if revision != "" {
		command = append(command, revision)
	}
	if path == "" {
		prefix, err := r.runGit("rev-parse", "--show-prefix")
		if err != nil {
			return nil, err
		}
		if prefix != "" {
			path = "."
		}
	}
	if path != "" {
		command = append(command, "--", path)
	} else {
		command = append(command, "--")
	}
	return command, nil
}

// Follow can traverse a rename outside a subdirectory even with a literal input
// path. Check the complete followed range before applying pagination or filters,
// which could otherwise hide the crossing commit. The normal Git timeout bounds
// this preflight; failures never return a partial or unverified history.
func (r *Registry) gitFollowBoundary(revision, path string) error {
	prefix, err := r.runGit("rev-parse", "--show-prefix")
	if err != nil {
		return err
	}
	command := []string{"log", "--follow", "--find-renames", "--no-ext-diff", "--no-textconv", "--no-relative", "--format=", "--name-status", "-z"}
	if prefix == "" {
		// At the repository root only the single-file contract needs checking.
		command = append(command, "-n", "1")
	}
	command = append(command, revision, "--", path)
	output, err := r.runGit(command...)
	if err != nil {
		return fmt.Errorf("cannot verify follow workspace boundary: %w; open the repository root for full history", err)
	}
	fields := strings.Split(output, "\x00")
	first := true
	for i := 0; i < len(fields); {
		status := strings.TrimLeft(fields[i], "\r\n")
		i++
		if status == "" {
			continue
		}
		count := 1
		if status[0] == 'R' || status[0] == 'C' {
			count = 2
		}
		if !strings.ContainsRune("ACDMRTUXB", rune(status[0])) || i+count > len(fields) {
			return fmt.Errorf("cannot verify follow workspace boundary; open the repository root for full history")
		}
		if first && fields[i+count-1] != prefix+path {
			return fmt.Errorf("follow requires one file path, not a directory")
		}
		first = false
		for _, name := range fields[i : i+count] {
			if name == "" || prefix != "" && !strings.HasPrefix(name, prefix) {
				return fmt.Errorf("follow crosses the workspace boundary; open the repository root for full history")
			}
		}
		i += count
	}
	return nil
}
