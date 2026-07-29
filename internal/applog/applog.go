package applog

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu     sync.Mutex
	writer io.Writer
	file   *os.File
)

func Init() error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable path: %w", err)
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable symlink: %w", err)
	}

	baseName := strings.TrimSuffix(filepath.Base(executable), filepath.Ext(executable))
	logPath := filepath.Join(filepath.Dir(executable), baseName+".log")

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}

	mu.Lock()
	file = logFile
	writer = io.MultiWriter(os.Stdout, logFile)
	mu.Unlock()

	Entry("applog", "Init", "logPath=%s", logPath)
	return nil
}

func Close() error {
	Entry("applog", "Close", "start")
	mu.Lock()
	defer mu.Unlock()
	if file == nil {
		return nil
	}
	err := file.Close()
	file = nil
	writer = nil
	return err
}

func Entry(module, function, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	line := fmt.Sprintf(
		"%s | %s - %s - %s",
		time.Now().Format("2006-01-02 15:04:05"),
		module,
		function,
		message,
	)

	mu.Lock()
	defer mu.Unlock()
	if writer == nil {
		fmt.Println(line)
		return
	}
	fmt.Fprintln(writer, line)
}

func Error(module, function string, err error) {
	if err == nil {
		return
	}
	Entry(module, function, "error: %v", err)
}

// Writer returns the shared console and log-file writer for external tools.
func Writer() io.Writer {
	mu.Lock()
	defer mu.Unlock()
	if writer == nil {
		return os.Stdout
	}
	return writer
}
