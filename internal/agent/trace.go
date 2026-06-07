package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"code-review-agent/internal/llm"
)

const traceFooter = "]\n"

type traceLog struct {
	path    string
	newFile bool
	mu      sync.Mutex
}

type traceChatMessage struct {
	Role    llm.Role `json:"role"`
	Content string   `json:"content"`
}

func newTraceLog(dir string) (*traceLog, error) {
	if dir == "" {
		dir = "log_sessions"
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	base := time.Now().Format("20060102-150405")
	for i := 0; i < 1000; i++ {
		name := base + ".json"
		if i > 0 {
			name = fmt.Sprintf("%s-%03d.json", base, i)
		}
		path := filepath.Join(dir, name)
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := file.WriteString("[\n" + traceFooter); err != nil {
			_ = file.Close()
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		return &traceLog{path: path, newFile: true}, nil
	}
	return nil, fmt.Errorf("cannot create unique trace log in %s", dir)
}

func resumeTraceLog(path string) (*traceLog, error) {
	if path == "" {
		return nil, fmt.Errorf("trace path is empty")
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		return &traceLog{path: path}, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := file.WriteString("[\n" + traceFooter); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	return &traceLog{path: path, newFile: true}, nil
}

func (l *traceLog) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

func (l *traceLog) IsNewFile() bool {
	return l != nil && l.newFile
}

func (l *traceLog) AppendMessage(message llm.Message) error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	payload, err := json.Marshal(traceChatMessage{Role: message.Role, Content: message.Content})
	if err != nil {
		return err
	}
	file, err := os.OpenFile(l.path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	footerBytes := []byte(traceFooter)
	if info.Size() < int64(len(footerBytes)) {
		return fmt.Errorf("trace log footer missing: %s", l.path)
	}
	footerAt := info.Size() - int64(len(footerBytes))
	tail := make([]byte, len(footerBytes))
	if _, err := file.ReadAt(tail, footerAt); err != nil {
		return err
	}
	if string(tail) != traceFooter {
		return fmt.Errorf("trace log footer invalid: %s", l.path)
	}
	prefixLen := minInt64(64, footerAt)
	prefix := make([]byte, prefixLen)
	if prefixLen > 0 {
		if _, err := file.ReadAt(prefix, footerAt-prefixLen); err != nil {
			return err
		}
	}
	hasMessages := !strings.HasSuffix(string(prefix), "[\n")
	if _, err := file.Seek(footerAt, 0); err != nil {
		return err
	}
	entry := "  " + string(payload) + "\n" + traceFooter
	if hasMessages {
		entry = ",\n" + entry
	}
	_, err = file.WriteString(entry)
	return err
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
