package tools

import (
	"encoding/json"
	pathpkg "path"
	"strings"
)

type fileReviewUpdateArgs struct {
	Path     string                 `json:"path"`
	Paths    []string               `json:"paths"`
	Dir      string                 `json:"dir"`
	Dirs     []string               `json:"dirs"`
	Suffix   string                 `json:"suffix"`
	Suffixes []string               `json:"suffixes"`
	Pattern  string                 `json:"pattern"`
	Patterns []string               `json:"patterns"`
	Status   string                 `json:"status"`
	Note     string                 `json:"note"`
	Items    []fileReviewUpdateItem `json:"items"`
}

type fileReviewUpdateItem struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type fileReviewBatchResult struct {
	Total   int          `json:"total"`
	Added   int          `json:"added"`
	Updated int          `json:"updated"`
	Sample  []FileReview `json:"sample,omitempty"`
}

type variableReviewUpdateArgs struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Status string `json:"status"`
	Note   string `json:"note"`
}

type reviewStateArgs struct {
	Limit int `json:"limit"`
}

type flowReviewUpdateArgs struct {
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	Entry     string   `json:"entry"`
	Files     []string `json:"files"`
	Variables []string `json:"variables"`
	Evidence  string   `json:"evidence"`
	NextStep  string   `json:"next_step"`
	Note      string   `json:"note"`
}

type flowReviewDeleteArgs struct {
	Name string `json:"name"`
}

func (r *Registry) fileReviewUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[fileReviewUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	items := r.normalizeFileReviewItems(args)
	if len(items) > 1 {
		return r.fileReviewUpdateBatch(items)
	}
	if len(items) == 1 {
		args.Path = items[0].Path
		args.Status = items[0].Status
		args.Note = items[0].Note
	}
	if len(items) == 0 && hasFileReviewSelectors(args) {
		return Result{OK: true, Data: fileReviewBatchResult{}, Message: "file review selector matched no files"}
	}
	if args.Path == "" {
		return Result{OK: false, Error: "path is required"}
	}
	if args.Status == "" {
		args.Status = "reviewed"
	}
	for i := range r.files {
		if r.files[i].Path == args.Path {
			r.files[i].Status = args.Status
			if args.Note != "" {
				r.files[i].Note = args.Note
			}
			return Result{OK: true, Data: r.files[i], Message: "file review updated"}
		}
	}
	item := FileReview{Path: args.Path, Status: args.Status, Note: args.Note}
	r.files = append(r.files, item)
	return Result{OK: true, Data: item, Message: "file review added"}
}

func normalizeFileReviewItems(args fileReviewUpdateArgs) []fileReviewUpdateItem {
	if len(args.Items) > 0 {
		items := make([]fileReviewUpdateItem, 0, len(args.Items))
		for _, item := range args.Items {
			if item.Status == "" {
				item.Status = args.Status
			}
			if item.Note == "" {
				item.Note = args.Note
			}
			items = append(items, item)
		}
		return items
	}
	if len(args.Paths) > 0 {
		items := make([]fileReviewUpdateItem, 0, len(args.Paths))
		for _, path := range args.Paths {
			items = append(items, fileReviewUpdateItem{Path: path, Status: args.Status, Note: args.Note})
		}
		return items
	}
	if args.Path != "" {
		return []fileReviewUpdateItem{{Path: args.Path, Status: args.Status, Note: args.Note}}
	}
	selectorPaths := selectPathsByFileReviewArgs(args, nil)
	if len(selectorPaths) > 0 {
		items := make([]fileReviewUpdateItem, 0, len(selectorPaths))
		for _, filePath := range selectorPaths {
			items = append(items, fileReviewUpdateItem{Path: filePath, Status: args.Status, Note: args.Note})
		}
		return items
	}
	return nil
}

func normalizeFileReviewItemsWithRegistry(args fileReviewUpdateArgs, files []FileReview) []fileReviewUpdateItem {
	items := normalizeFileReviewItems(args)
	if len(items) > 0 || !hasFileReviewSelectors(args) {
		return items
	}
	selectorPaths := selectPathsByFileReviewArgs(args, files)
	items = make([]fileReviewUpdateItem, 0, len(selectorPaths))
	for _, filePath := range selectorPaths {
		items = append(items, fileReviewUpdateItem{Path: filePath, Status: args.Status, Note: args.Note})
	}
	return items
}

func (r *Registry) normalizeFileReviewItems(args fileReviewUpdateArgs) []fileReviewUpdateItem {
	items := normalizeFileReviewItems(args)
	if len(items) > 0 || !hasFileReviewSelectors(args) {
		for i := range items {
			items[i].Path = normalizeInventoryPath(items[i].Path)
		}
		return items
	}
	selectorPaths := r.selectInventoryPathsByFileReviewArgs(args)
	items = make([]fileReviewUpdateItem, 0, len(selectorPaths))
	for _, filePath := range selectorPaths {
		items = append(items, fileReviewUpdateItem{Path: filePath, Status: args.Status, Note: args.Note})
	}
	return items
}

func (r *Registry) selectInventoryPathsByFileReviewArgs(args fileReviewUpdateArgs) []string {
	dirs := append([]string(nil), args.Dirs...)
	if args.Dir != "" {
		dirs = append(dirs, args.Dir)
	}
	suffixes := append([]string(nil), args.Suffixes...)
	if args.Suffix != "" {
		suffixes = append(suffixes, args.Suffix)
	}
	patterns := append([]string(nil), args.Patterns...)
	if args.Pattern != "" {
		patterns = append(patterns, args.Pattern)
	}
	for i := range dirs {
		dirs[i] = normalizeSelectorDir(dirs[i])
	}
	for i := range suffixes {
		suffixes[i] = normalizeSelectorSuffix(suffixes[i])
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, file := range r.inventory {
		if !matchesFileSelectors(file.Path, dirs, suffixes, patterns) {
			continue
		}
		if _, ok := seen[file.Path]; ok {
			continue
		}
		seen[file.Path] = struct{}{}
		paths = append(paths, file.Path)
	}
	return paths
}

func hasFileReviewSelectors(args fileReviewUpdateArgs) bool {
	return args.Dir != "" || len(args.Dirs) > 0 || args.Suffix != "" || len(args.Suffixes) > 0 || args.Pattern != "" || len(args.Patterns) > 0
}

func selectPathsByFileReviewArgs(args fileReviewUpdateArgs, files []FileReview) []string {
	if len(files) == 0 {
		return nil
	}
	dirs := append([]string(nil), args.Dirs...)
	if args.Dir != "" {
		dirs = append(dirs, args.Dir)
	}
	suffixes := append([]string(nil), args.Suffixes...)
	if args.Suffix != "" {
		suffixes = append(suffixes, args.Suffix)
	}
	patterns := append([]string(nil), args.Patterns...)
	if args.Pattern != "" {
		patterns = append(patterns, args.Pattern)
	}
	for i := range dirs {
		dirs[i] = normalizeSelectorDir(dirs[i])
	}
	for i := range suffixes {
		suffixes[i] = normalizeSelectorSuffix(suffixes[i])
	}
	seen := map[string]struct{}{}
	var paths []string
	for _, file := range files {
		if !matchesFileSelectors(file.Path, dirs, suffixes, patterns) {
			continue
		}
		if _, ok := seen[file.Path]; ok {
			continue
		}
		seen[file.Path] = struct{}{}
		paths = append(paths, file.Path)
	}
	return paths
}

func normalizeSelectorDir(dir string) string {
	dir = strings.TrimSpace(strings.ReplaceAll(dir, "\\", "/"))
	dir = strings.TrimPrefix(dir, "./")
	dir = strings.Trim(dir, "/")
	return dir
}

func normalizeSelectorSuffix(suffix string) string {
	suffix = strings.TrimSpace(suffix)
	if suffix == "" || suffix == "*" {
		return suffix
	}
	if !strings.HasPrefix(suffix, ".") && !strings.HasPrefix(suffix, "*") {
		suffix = "." + suffix
	}
	return strings.ToLower(strings.TrimPrefix(suffix, "*"))
}

func matchesFileSelectors(filePath string, dirs, suffixes, patterns []string) bool {
	if len(dirs) == 0 && len(suffixes) == 0 && len(patterns) == 0 {
		return false
	}
	return matchesAnyDir(filePath, dirs) || matchesAnySuffix(filePath, suffixes) || matchesAnyPattern(filePath, patterns)
}

func matchesAnyDir(filePath string, dirs []string) bool {
	for _, dir := range dirs {
		if dir == "" || dir == "." {
			return true
		}
		if filePath == dir || strings.HasPrefix(filePath, dir+"/") {
			return true
		}
	}
	return false
}

func matchesAnySuffix(filePath string, suffixes []string) bool {
	lowerPath := strings.ToLower(filePath)
	for _, suffix := range suffixes {
		if suffix == "*" {
			return true
		}
		if suffix != "" && strings.HasSuffix(lowerPath, suffix) {
			return true
		}
	}
	return false
}

func matchesAnyPattern(filePath string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			return true
		}
		if ok, _ := pathpkg.Match(pattern, filePath); ok {
			return true
		}
	}
	return false
}

func (r *Registry) fileReviewUpdateBatch(items []fileReviewUpdateItem) Result {
	result := fileReviewBatchResult{Total: len(items)}
	for _, item := range items {
		if item.Path == "" {
			return Result{OK: false, Error: "path is required for every item"}
		}
		if item.Status == "" {
			item.Status = "reviewed"
		}
		updated := false
		for i := range r.files {
			if r.files[i].Path == item.Path {
				r.files[i].Status = item.Status
				if item.Note != "" {
					r.files[i].Note = item.Note
				}
				result.Updated++
				result.Sample = appendFileReviewSample(result.Sample, r.files[i])
				updated = true
				break
			}
		}
		if updated {
			continue
		}
		file := FileReview{Path: item.Path, Status: item.Status, Note: item.Note}
		r.files = append(r.files, file)
		result.Added++
		result.Sample = appendFileReviewSample(result.Sample, file)
	}
	return Result{OK: true, Data: result, Message: "file review batch updated"}
}

func appendFileReviewSample(sample []FileReview, item FileReview) []FileReview {
	if len(sample) >= 20 {
		return sample
	}
	return append(sample, item)
}

func (r *Registry) variableReviewUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[variableReviewUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Name == "" {
		return Result{OK: false, Error: "name is required"}
	}
	if args.Status == "" {
		args.Status = "tracking"
	}
	for i := range r.variables {
		if r.variables[i].Name == args.Name && r.variables[i].Path == args.Path {
			r.variables[i].Status = args.Status
			if args.Note != "" {
				r.variables[i].Note = args.Note
			}
			return Result{OK: true, Data: r.variables[i], Message: "variable review updated"}
		}
	}
	item := VariableReview{Name: args.Name, Path: args.Path, Status: args.Status, Note: args.Note}
	r.variables = append(r.variables, item)
	return Result{OK: true, Data: item, Message: "variable review added"}
}

func (r *Registry) flowReviewUpdate(raw json.RawMessage) Result {
	args, err := decodeArgs[flowReviewUpdateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Name == "" {
		return Result{OK: false, Error: "name is required"}
	}
	if args.Status == "" {
		args.Status = "tracking"
	}
	for i := range r.flows {
		if r.flows[i].Name == args.Name {
			if isClosedFlowStatus(args.Status) {
				removed := r.flows[i]
				r.flows = append(r.flows[:i], r.flows[i+1:]...)
				removed.Status = args.Status
				return Result{OK: true, Data: removed, Message: "flow review closed and removed"}
			}
			r.flows[i].Status = args.Status
			if args.Entry != "" {
				r.flows[i].Entry = args.Entry
			}
			if len(args.Files) > 0 {
				r.flows[i].Files = args.Files
			}
			if len(args.Variables) > 0 {
				r.flows[i].Variables = args.Variables
			}
			if args.Evidence != "" {
				r.flows[i].Evidence = args.Evidence
			}
			if args.NextStep != "" {
				r.flows[i].NextStep = args.NextStep
			}
			if args.Note != "" {
				r.flows[i].Note = args.Note
			}
			return Result{OK: true, Data: r.flows[i], Message: "flow review updated"}
		}
	}
	if isClosedFlowStatus(args.Status) {
		return Result{OK: true, Message: "flow review already closed; nothing added"}
	}
	item := FlowReview{Name: args.Name, Status: args.Status, Entry: args.Entry, Files: args.Files, Variables: args.Variables, Evidence: args.Evidence, NextStep: args.NextStep, Note: args.Note}
	r.flows = append(r.flows, item)
	return Result{OK: true, Data: item, Message: "flow review added"}
}

func (r *Registry) flowReviewDelete(raw json.RawMessage) Result {
	args, err := decodeArgs[flowReviewDeleteArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Name == "" {
		return Result{OK: false, Error: "name is required"}
	}
	for i := range r.flows {
		if r.flows[i].Name == args.Name {
			removed := r.flows[i]
			r.flows = append(r.flows[:i], r.flows[i+1:]...)
			return Result{OK: true, Data: removed, Message: "flow review deleted"}
		}
	}
	return Result{OK: false, Error: "flow not found"}
}

func isClosedFlowStatus(status string) bool {
	switch status {
	case "reviewed", "benign", "done", "closed":
		return true
	default:
		return false
	}
}

func (r *Registry) reviewState(raw json.RawMessage) Result {
	args, err := decodeArgs[reviewStateArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Limit <= 0 {
		args.Limit = 80
	}
	s := r.Snapshot()
	if len(s.Files) > args.Limit {
		s.Files = s.Files[:args.Limit]
	}
	if len(s.Variables) > args.Limit {
		s.Variables = s.Variables[:args.Limit]
	}
	if len(s.Flows) > args.Limit {
		s.Flows = s.Flows[:args.Limit]
	}
	return Result{OK: true, Data: s}
}
