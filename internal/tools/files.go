package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type listFilesArgs struct {
	Root          string `json:"root"`
	Pattern       string `json:"pattern"`
	MaxDepth      int    `json:"max_depth"`
	IncludeHidden bool   `json:"include_hidden"`
	Limit         int    `json:"limit"`
}

type fileEntry struct {
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size,omitempty"`
}

func (r *Registry) listFiles(raw json.RawMessage) Result {
	args, err := decodeArgs[listFilesArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Limit <= 0 {
		args.Limit = 500
	}
	root, err := r.safePath(args.Root)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	baseDepth := depth(root)
	var out []fileEntry
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if !args.IncludeHidden && strings.HasPrefix(name, ".") && path != root {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if args.MaxDepth > 0 && depth(path)-baseDepth > args.MaxDepth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(r.workspace, path)
		rel = filepath.ToSlash(rel)
		if args.Pattern != "" {
			matched, err := matchPatternInsensitive(args.Pattern, filepath.Base(path))
			if err != nil || !matched {
				return nil
			}
		}
		info, _ := d.Info()
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		out = append(out, fileEntry{Path: rel, IsDir: d.IsDir(), Size: size})
		if len(out) >= args.Limit {
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return Result{OK: false, Error: err.Error()}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return Result{OK: true, Data: out, Trunc: err == io.EOF}
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

type fileLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

type readFileResult struct {
	Path       string     `json:"path"`
	Offset     int        `json:"offset"`
	Limit      int        `json:"limit"`
	TotalLines int        `json:"total_lines"`
	Lines      []fileLine `json:"lines"`
}

func (r *Registry) readFile(raw json.RawMessage) Result {
	args, err := decodeArgs[readFileArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if args.Path == "" {
		return Result{OK: false, Error: "path is required"}
	}
	if args.Offset <= 0 {
		args.Offset = 1
	}
	if args.Limit <= 0 {
		args.Limit = 120
	}
	path, resolvedPath, err := r.resolveReadableFile(args.Path)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	var lines []fileLine
	truncated := false
	for scanner.Scan() {
		lineNo++
		if lineNo < args.Offset {
			continue
		}
		if len(lines) >= args.Limit {
			truncated = true
			continue
		}
		lines = append(lines, fileLine{Line: lineNo, Text: scanner.Text()})
	}
	if err := scanner.Err(); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Data: readFileResult{Path: resolvedPath, Offset: args.Offset, Limit: args.Limit, TotalLines: lineNo, Lines: lines}, Trunc: truncated}
}

func (r *Registry) resolveReadableFile(input string) (string, string, error) {
	input = strings.TrimSpace(strings.Trim(input, "\"'"))
	if input == "" {
		return "", "", fmt.Errorf("path is required")
	}
	if abs, rel, ok := r.existingFile(input); ok {
		return abs, rel, nil
	}
	for _, candidate := range r.pathRecoveryCandidates(input) {
		if abs, rel, ok := r.existingFile(candidate); ok {
			return abs, rel, nil
		}
	}
	baseMatches := r.inventoryBaseMatches(input)
	if len(baseMatches) == 1 {
		if abs, rel, ok := r.existingFile(baseMatches[0]); ok {
			return abs, rel, nil
		}
	}
	if len(baseMatches) > 1 {
		return "", "", fmt.Errorf("path not found and basename is ambiguous: %s; candidates: %s", input, strings.Join(limitStrings(baseMatches, 8), ", "))
	}
	return "", "", fmt.Errorf("path not found in workspace: %s. Use an exact relative path from review_state/list_files/search_content, not a guessed absolute path", input)
}

func (r *Registry) existingFile(candidate string) (string, string, bool) {
	abs, err := r.safePath(candidate)
	if err != nil {
		return "", "", false
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", "", false
	}
	rel, err := filepath.Rel(r.workspace, abs)
	if err != nil {
		return "", "", false
	}
	return abs, filepath.ToSlash(rel), true
}

func (r *Registry) pathRecoveryCandidates(input string) []string {
	normalized := strings.TrimSpace(strings.ReplaceAll(input, "\\", "/"))
	workspaceBase := filepath.ToSlash(filepath.Base(r.workspace))
	var candidates []string
	addCandidate := func(candidate string) {
		candidate = strings.TrimSpace(strings.ReplaceAll(candidate, "\\", "/"))
		if candidate == "" {
			return
		}
		for _, existing := range candidates {
			if strings.EqualFold(existing, candidate) {
				return
			}
		}
		candidates = append(candidates, candidate)
	}
	if workspaceBase != "" {
		needle := strings.ToLower(workspaceBase) + "/"
		if idx := strings.LastIndex(strings.ToLower(normalized), needle); idx >= 0 {
			addCandidate(normalized[idx+len(needle):])
		}
	}
	for _, marker := range []string{"src/", "app/", "internal/", "pkg/", "cmd/", "lib/", "resources/", "config/", "configs/"} {
		if idx := strings.Index(strings.ToLower(normalized), marker); idx >= 0 {
			addCandidate(normalized[idx:])
		}
	}
	return candidates
}

func (r *Registry) inventoryBaseMatches(input string) []string {
	base := pathpkg.Base(strings.ReplaceAll(strings.TrimSpace(input), "\\", "/"))
	if base == "" || base == "." || base == "/" {
		return nil
	}
	var matches []string
	for _, file := range r.inventory {
		if strings.EqualFold(pathpkg.Base(file.Path), base) {
			matches = append(matches, file.Path)
		}
	}
	return matches
}

func limitStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	out := append([]string(nil), values[:limit]...)
	out = append(out, fmt.Sprintf("...and %d more", len(values)-limit))
	return out
}

type searchArgs struct {
	Query           string   `json:"query"`
	Keyword         string   `json:"keyword"`
	Text            string   `json:"text"`
	Content         string   `json:"content"`
	Needle          string   `json:"needle"`
	Term            string   `json:"term"`
	Pattern         string   `json:"pattern"`
	Mode            string   `json:"mode"`
	Root            string   `json:"root"`
	Dir             string   `json:"dir"`
	Directory       string   `json:"directory"`
	Include         string   `json:"include"`
	Includes        []string `json:"includes"`
	FilePattern     string   `json:"file_pattern"`
	FilePatterns    []string `json:"file_patterns"`
	Path            string   `json:"path"`
	Limit           int      `json:"limit"`
	CaseInsensitive *bool    `json:"case_insensitive"`
	CaseSensitive   bool     `json:"case_sensitive"`
}

type searchHit struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Snippet string `json:"snippet"`
}

func (r *Registry) searchContent(raw json.RawMessage) Result {
	args, err := decodeArgs[searchArgs](raw)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	args.normalize()
	if args.Query == "" {
		return Result{OK: false, Error: "query is required"}
	}
	if args.Mode == "" {
		args.Mode = "literal"
	}
	caseInsensitive := args.caseInsensitive()
	if args.Limit <= 0 {
		args.Limit = 100
	}
	root, err := r.safePath(args.Root)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	var re *regexp.Regexp
	if args.Mode == "regex" {
		pattern := args.Query
		if caseInsensitive && !strings.HasPrefix(pattern, "(?i)") {
			pattern = "(?i)" + pattern
		}
		re, err = regexp.Compile(pattern)
		if err != nil {
			return Result{OK: false, Error: err.Error()}
		}
	}
	var hits []searchHit
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if len(args.includePatterns()) > 0 {
			matched, err := matchAnyIncludePattern(args.includePatterns(), rel)
			if err != nil || !matched {
				return nil
			}
		}
		fileHits, err := r.searchFile(path, args.Query, args.Mode, re, args.Limit-len(hits), caseInsensitive)
		if err != nil {
			return nil
		}
		hits = append(hits, fileHits...)
		if len(hits) >= args.Limit {
			return io.EOF
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Data: hits, Trunc: err == io.EOF}
}

func (a *searchArgs) normalize() {
	if a.Query == "" {
		a.Query = firstNonEmpty(a.Keyword, a.Text, a.Content, a.Needle, a.Term)
	}
	if a.Query == "" && strings.TrimSpace(a.Pattern) != "" {
		if looksLikeIncludePattern(a.Pattern) {
			a.Include = firstNonEmpty(a.Include, a.Pattern)
		} else {
			a.Query = strings.TrimSpace(a.Pattern)
			if a.Mode == "" {
				a.Mode = "regex"
			}
		}
	} else if a.Include == "" && a.FilePattern == "" && looksLikeIncludePattern(a.Pattern) {
		a.Include = a.Pattern
	}
	if a.Root == "" {
		a.Root = firstNonEmpty(a.Path, a.Dir, a.Directory)
	}
	if a.Include == "" {
		a.Include = a.FilePattern
	}
	if len(a.Includes) == 0 {
		a.Includes = a.FilePatterns
	}
	a.Mode = normalizeSearchMode(a.Mode)
}

func (a searchArgs) caseInsensitive() bool {
	if a.CaseSensitive {
		return false
	}
	if a.CaseInsensitive == nil {
		return true
	}
	return *a.CaseInsensitive
}

func normalizeSearchMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "literal", "text", "contains", "substring", "exact":
		return "literal"
	case "regex", "regexp", "regular_expression", "regular-expression":
		return "regex"
	case "fuzzy", "fuzz", "keyword", "keywords":
		return "fuzzy"
	default:
		return "literal"
	}
}

func looksLikeIncludePattern(pattern string) bool {
	pattern = strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	if pattern == "" {
		return false
	}
	if strings.Contains(pattern, "/") {
		return true
	}
	if strings.ContainsAny(pattern, "*?") {
		return true
	}
	for _, suffix := range []string{".go", ".java", ".xml", ".sql", ".js", ".ts", ".tsx", ".jsx", ".vue", ".py", ".php", ".cs", ".cpp", ".c", ".h", ".yaml", ".yml", ".properties"} {
		if strings.HasSuffix(strings.ToLower(pattern), suffix) {
			return true
		}
	}
	return false
}

func (a searchArgs) includePatterns() []string {
	var patterns []string
	appendPatterns := func(values ...string) {
		for _, value := range values {
			for _, pattern := range splitPatternList(value) {
				patterns = append(patterns, pattern)
			}
		}
	}
	appendPatterns(a.Includes...)
	appendPatterns(a.Include)
	return patterns
}

func splitPatternList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t'
	})
	var out []string
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (r *Registry) searchFile(path, query, mode string, re *regexp.Regexp, limit int, caseInsensitive bool) ([]searchHit, error) {
	if limit <= 0 {
		return nil, nil
	}
	if mode == "regex" {
		return r.searchRegexFile(path, re, limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	var hits []searchHit
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		col := matchColumn(line, query, mode, re, caseInsensitive)
		if col == 0 {
			continue
		}
		rel, _ := filepath.Rel(r.workspace, path)
		hits = append(hits, searchHit{Path: filepath.ToSlash(rel), Line: lineNo, Column: col, Snippet: strings.TrimSpace(line)})
		if len(hits) >= limit {
			break
		}
	}
	return hits, scanner.Err()
}

func (r *Registry) searchRegexFile(path string, re *regexp.Regexp, limit int) ([]searchHit, error) {
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	content := string(contentBytes)
	matches := re.FindAllStringIndex(content, limit)
	if len(matches) == 0 {
		return nil, nil
	}
	rel, _ := filepath.Rel(r.workspace, path)
	var hits []searchHit
	for _, match := range matches {
		if match[0] == match[1] {
			continue
		}
		line, column := lineColumnAt(content, match[0])
		hits = append(hits, searchHit{Path: filepath.ToSlash(rel), Line: line, Column: column, Snippet: strings.TrimSpace(lineSnippetAt(content, match[0]))})
		if len(hits) >= limit {
			break
		}
	}
	return hits, nil
}

func lineColumnAt(content string, offset int) (int, int) {
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	line := 1
	lineStart := 0
	for i, r := range content[:offset] {
		if r == '\n' {
			line++
			lineStart = i + 1
		}
	}
	return line, offset - lineStart + 1
}

func lineSnippetAt(content string, offset int) string {
	if offset < 0 {
		offset = 0
	}
	if offset > len(content) {
		offset = len(content)
	}
	start := strings.LastIndex(content[:offset], "\n") + 1
	end := strings.Index(content[offset:], "\n")
	if end < 0 {
		end = len(content)
	} else {
		end += offset
	}
	return content[start:end]
}

func matchColumn(line, query, mode string, re *regexp.Regexp, caseInsensitive bool) int {
	switch mode {
	case "regex":
		loc := re.FindStringIndex(line)
		if loc == nil {
			return 0
		}
		return loc[0] + 1
	case "fuzzy":
		idx := fuzzyIndex(strings.ToLower(line), strings.ToLower(query))
		return idx + 1
	default:
		if caseInsensitive {
			idx := strings.Index(strings.ToLower(line), strings.ToLower(query))
			if idx < 0 {
				return 0
			}
			return idx + 1
		}
		idx := strings.Index(line, query)
		if idx < 0 {
			return 0
		}
		return idx + 1
	}
}

func matchIncludePattern(include, rel string) (bool, error) {
	include = strings.ReplaceAll(strings.TrimSpace(include), "\\", "/")
	if include == "" {
		return true, nil
	}
	matches, err := matchIncludePatternExact(include, rel)
	if err != nil {
		return false, err
	}
	if matches {
		return true, nil
	}
	return matchIncludePatternExact(strings.ToLower(include), strings.ToLower(rel))
}

func matchIncludePatternExact(include, rel string) (bool, error) {
	base := pathpkg.Base(rel)
	if strings.Contains(include, "/") {
		return matchGlob(include, rel)
	}
	matchedBase, err := matchGlob(include, base)
	if err != nil || matchedBase {
		return matchedBase, err
	}
	return matchGlob(include, rel)
}

func matchGlob(pattern, value string) (bool, error) {
	re, err := globPatternRegex(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(value), nil
}

func globPatternRegex(pattern string) (*regexp.Regexp, error) {
	pattern = strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func matchAnyIncludePattern(includes []string, rel string) (bool, error) {
	for _, include := range includes {
		matched, err := matchIncludePattern(include, rel)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func matchPatternInsensitive(pattern, value string) (bool, error) {
	pattern = strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	if pattern == "" {
		return true, nil
	}
	matched, err := pathpkg.Match(pattern, value)
	if err != nil {
		return false, err
	}
	if matched {
		return true, nil
	}
	return pathpkg.Match(strings.ToLower(pattern), strings.ToLower(value))
}

func fuzzyIndex(line, query string) int {
	if query == "" {
		return -1
	}
	if idx := fuzzyKeywordIndex(line, query); idx >= 0 {
		return idx
	}
	start := -1
	j := 0
	for i, r := range line {
		if j < len(query) && byte(r) == query[j] {
			if start == -1 {
				start = i
			}
			j++
			if j == len(query) {
				return start
			}
		}
	}
	return -1
}

func fuzzyKeywordIndex(line, query string) int {
	tokens := fuzzyTokens(query)
	if len(tokens) <= 1 {
		return -1
	}
	best := -1
	matched := 0
	for _, token := range tokens {
		idx := strings.Index(line, token)
		if idx < 0 {
			continue
		}
		matched++
		if best < 0 || idx < best {
			best = idx
		}
	}
	if matched == 0 {
		return -1
	}
	return best
}

func fuzzyTokens(query string) []string {
	parts := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		switch {
		case r >= 'a' && r <= 'z':
			return false
		case r >= '0' && r <= '9':
			return false
		case r == '_':
			return false
		default:
			return true
		}
	})
	seen := map[string]struct{}{}
	var tokens []string
	for _, part := range parts {
		if len(part) < 2 {
			continue
		}
		if _, ok := seen[part]; ok {
			continue
		}
		seen[part] = struct{}{}
		tokens = append(tokens, part)
	}
	return tokens
}

func depth(path string) int {
	clean := filepath.Clean(path)
	if clean == string(os.PathSeparator) || clean == "." {
		return 0
	}
	return strings.Count(clean, string(os.PathSeparator))
}

func formatLine(lineNo int, text string) string {
	return fmt.Sprintf("%d: %s", lineNo, text)
}
