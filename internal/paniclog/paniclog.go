package paniclog

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Recover writes a panic dump and exits. It must be deferred in every goroutine
// that can run application code; recover only catches panics from its goroutine.
func Recover() {
	RecoverWithContext("")
}

func RecoverWithContext(context string) {
	recovered := recover()
	if recovered == nil {
		return
	}

	path, err := writePanicDump(context, recovered)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to write panic log: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "panic logged to %s, exiting...\n", path)
	}
	os.Exit(1)
}

func writePanicDump(context string, recovered any) (string, error) {
	logDir := "logs"
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return "", err
	}

	timestamp := time.Now().Format("20060102-150405-000000000")
	path := filepath.Join(logDir, fmt.Sprintf("panic-%s.log", timestamp))
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer file.Close()

	wd, _ := os.Getwd()
	fmt.Fprintf(file, "Panic Log\n")
	fmt.Fprintf(file, "Time: %s\n", time.Now().Format(time.RFC3339Nano))
	fmt.Fprintf(file, "Context: %s\n", context)
	fmt.Fprintf(file, "Binary: %s\n", os.Args[0])
	fmt.Fprintf(file, "WorkingDir: %s\n", wd)
	fmt.Fprintf(file, "Args: %v\n", os.Args)
	fmt.Fprintf(file, "\nPanic: %v\n\n", recovered)
	fmt.Fprintf(file, "Stack trace (all goroutines):\n%s\n", allStacks())
	return path, nil
}

func allStacks() string {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		if len(buf) >= 16*1024*1024 {
			return string(buf)
		}
		buf = make([]byte, len(buf)*2)
	}
}
