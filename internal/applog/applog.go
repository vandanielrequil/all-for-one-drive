package applog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	mu               sync.Mutex
	writer           io.Writer
	file             *os.File
	availabilityFile *os.File
	mapFile          *os.File
	errorPath        string
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
	executableDir := filepath.Dir(executable)
	logPath := filepath.Join(executableDir, baseName+".log")
	availabilityPath := filepath.Join(executableDir, "availability.log")
	mapPath := filepath.Join(executableDir, "all-for-one-map.txt")
	errorPath = filepath.Join(executableDir, baseName+".error")
	if err := os.Remove(errorPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove previous error marker %q: %w", errorPath, err)
	}

	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	availabilityLog, err := os.OpenFile(
		availabilityPath,
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0o644,
	)
	if err != nil {
		logFile.Close()
		return fmt.Errorf("open availability log %q: %w", availabilityPath, err)
	}
	uploadMap, err := os.OpenFile(mapPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logFile.Close()
		availabilityLog.Close()
		return fmt.Errorf("open upload map %q: %w", mapPath, err)
	}

	mu.Lock()
	file = logFile
	availabilityFile = availabilityLog
	mapFile = uploadMap
	writer = io.MultiWriter(os.Stdout, logFile)
	mu.Unlock()

	Entry(
		"applog",
		"Init",
		"logPath=%s availabilityPath=%s mapPath=%s errorPath=%s",
		logPath,
		availabilityPath,
		mapPath,
		errorPath,
	)
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
	if availabilityFile != nil {
		err = errors.Join(err, availabilityFile.Close())
	}
	if mapFile != nil {
		err = errors.Join(err, mapFile.Close())
	}
	file = nil
	availabilityFile = nil
	mapFile = nil
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

// Availability writes one line to availability.log only.
func Availability(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	mu.Lock()
	defer mu.Unlock()
	if availabilityFile != nil {
		fmt.Fprintln(availabilityFile, line)
	}
}

// UploadMap appends one line to all-for-one-map.txt.
func UploadMap(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	mu.Lock()
	defer mu.Unlock()
	if mapFile != nil {
		fmt.Fprintln(mapFile, line)
	}
}

// WriteErrorMarker creates {binary}.error with the failure text.
// On success the marker must not exist; Init removes a leftover marker.
func WriteErrorMarker(err error) error {
	if err == nil {
		return nil
	}
	mu.Lock()
	path := errorPath
	mu.Unlock()
	if path == "" {
		executable, resolveErr := os.Executable()
		if resolveErr != nil {
			return resolveErr
		}
		executable, resolveErr = filepath.EvalSymlinks(executable)
		if resolveErr != nil {
			return resolveErr
		}
		baseName := strings.TrimSuffix(filepath.Base(executable), filepath.Ext(executable))
		path = filepath.Join(filepath.Dir(executable), baseName+".error")
	}
	return os.WriteFile(path, []byte(err.Error()+"\n"), 0o644)
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
