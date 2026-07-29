package archiver

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"all-for-one-drive/internal/applog"
)

const bytesPerMegabyte = 1024 * 1024

type Config struct {
	OutputDir        string `json:"outputDir"`
	MaxArchiveSizeMB int64  `json:"maxArchiveSizeMB"`
}

type Progress struct {
	SourceDir   string
	ArchivePath string
	FileCount   int
	Size        int64
}

type Summary struct {
	Directories int
	Archives    int
	Files       int
}

type Archiver struct {
	config   Config
	sevenZip string
}

type sourceFile struct {
	relativePath string
	size         int64
}

func New(config Config) (*Archiver, error) {
	applog.Entry("archiver", "New", "outputDir=%s maxArchiveSizeMB=%d", config.OutputDir, config.MaxArchiveSizeMB)
	if strings.TrimSpace(config.OutputDir) == "" {
		return nil, errors.New("outputDir is required")
	}
	if config.MaxArchiveSizeMB <= 0 {
		return nil, errors.New("maxArchiveSizeMB must be greater than zero")
	}

	outputDir, err := filepath.Abs(config.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory: %w", err)
	}
	sevenZip, err := findSevenZip()
	if err != nil {
		return nil, err
	}
	config.OutputDir = outputDir
	return &Archiver{config: config, sevenZip: sevenZip}, nil
}

// ArchiveDirectories archives each directory independently. Files from
// different directories are never placed in the same archive.
func (a *Archiver) ArchiveDirectories(
	ctx context.Context,
	inputDirs []string,
	password string,
	report func(Progress),
) (Summary, error) {
	applog.Entry("archiver", "ArchiveDirectories", "inputDirs=%v passwordSet=%t", inputDirs, password != "")
	if err := os.MkdirAll(a.config.OutputDir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("create archive output directory: %w", err)
	}

	var summary Summary
	for _, inputDir := range inputDirs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		absoluteDir, err := filepath.Abs(inputDir)
		if err != nil {
			return summary, fmt.Errorf("resolve input directory %q: %w", inputDir, err)
		}
		if samePath(absoluteDir, a.config.OutputDir) || pathWithin(absoluteDir, a.config.OutputDir) {
			return summary, fmt.Errorf("archive output directory must not be inside input directory %q", inputDir)
		}

		files, err := collectFiles(absoluteDir)
		if err != nil {
			return summary, err
		}
		if len(files) == 0 {
			fmt.Printf("Архиватор: %s пустая, пропуск\n", absoluteDir)
			continue
		}

		groups := groupFiles(files, a.config.MaxArchiveSizeMB*bytesPerMegabyte)
		directoryArchives := 0
		for _, group := range groups {
			created, err := a.archiveGroup(ctx, absoluteDir, group, password, report)
			if err != nil {
				return summary, err
			}
			directoryArchives += created
		}

		if err := clearDirectory(absoluteDir); err != nil {
			return summary, fmt.Errorf("clear archived directory %q: %w", absoluteDir, err)
		}
		summary.Directories++
		summary.Archives += directoryArchives
		summary.Files += len(files)
		fmt.Printf("Архиватор: %s очищена после успешной архивации\n", absoluteDir)
	}
	return summary, nil
}

func (a *Archiver) archiveGroup(
	ctx context.Context,
	inputDir string,
	files []sourceFile,
	password string,
	report func(Progress),
) (int, error) {
	applog.Entry("archiver", "archiveGroup", "inputDir=%s files=%d passwordSet=%t", inputDir, len(files), password != "")
	archivePath, err := a.nextArchivePath(inputDir, files[0])
	if err != nil {
		return 0, err
	}
	if err := a.runSevenZip(ctx, inputDir, archivePath, files, password); err != nil {
		return 0, err
	}

	info, err := os.Stat(archivePath)
	if err != nil {
		return 0, fmt.Errorf("inspect archive %q: %w", archivePath, err)
	}
	maxSize := a.config.MaxArchiveSizeMB * bytesPerMegabyte
	if info.Size() > maxSize && len(files) > 1 {
		if err := os.Remove(archivePath); err != nil {
			return 0, fmt.Errorf("remove oversized archive %q: %w", archivePath, err)
		}
		middle := len(files) / 2
		leftCount, err := a.archiveGroup(ctx, inputDir, files[:middle], password, report)
		if err != nil {
			return 0, err
		}
		rightCount, err := a.archiveGroup(ctx, inputDir, files[middle:], password, report)
		if err != nil {
			return 0, err
		}
		return leftCount + rightCount, nil
	}
	if info.Size() > maxSize && files[0].size <= maxSize {
		if err := os.Remove(archivePath); err != nil {
			return 0, fmt.Errorf("remove oversized archive %q: %w", archivePath, err)
		}
		return 0, fmt.Errorf(
			"single-file archive %q exceeds the configured limit (%d bytes)",
			archivePath,
			maxSize,
		)
	}

	if report != nil {
		report(Progress{
			SourceDir:   inputDir,
			ArchivePath: archivePath,
			FileCount:   len(files),
			Size:        info.Size(),
		})
	}
	return 1, nil
}

func (a *Archiver) runSevenZip(
	ctx context.Context,
	inputDir string,
	archivePath string,
	files []sourceFile,
	password string,
) error {
	applog.Entry("archiver", "runSevenZip", "inputDir=%s archivePath=%s files=%d passwordSet=%t sevenZip=%s", inputDir, archivePath, len(files), password != "", a.sevenZip)
	listFile, err := os.CreateTemp("", "all-for-one-7zip-*.txt")
	if err != nil {
		return fmt.Errorf("create 7-Zip file list: %w", err)
	}
	listPath := listFile.Name()
	defer os.Remove(listPath)

	for _, file := range files {
		if _, err := fmt.Fprintln(listFile, filepath.ToSlash(file.relativePath)); err != nil {
			listFile.Close()
			return fmt.Errorf("write 7-Zip file list: %w", err)
		}
	}
	if err := listFile.Close(); err != nil {
		return fmt.Errorf("close 7-Zip file list: %w", err)
	}

	fmt.Printf("7-Zip: %d файлов -> %s\n", len(files), archivePath)
	args := []string{
		"a",
		"-t7z",
		"-mx=0",
		"-scsUTF-8",
		"-y",
	}
	if password != "" {
		args = append(args, "-mhe=on", "-p"+password)
	}
	args = append(args, archivePath, "@"+listPath)
	applog.Entry("archiver", "runSevenZip", "command=%s %s", a.sevenZip, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, a.sevenZip, args...)
	cmd.Dir = inputDir
	cmd.Stdout = applog.Writer()
	cmd.Stderr = applog.Writer()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("7-Zip failed for %q: %w", inputDir, err)
	}
	return nil
}

func (a *Archiver) nextArchivePath(inputDir string, first sourceFile) (string, error) {
	applog.Entry("archiver", "nextArchivePath", "inputDir=%s firstFile=%s", inputDir, first.relativePath)
	directoryName := sanitizeName(filepath.Base(filepath.Clean(inputDir)))
	fileName := sanitizeName(strings.TrimSuffix(filepath.Base(first.relativePath), filepath.Ext(first.relativePath)))
	base := directoryName + "-" + fileName
	path := filepath.Join(a.config.OutputDir, base+".7z")
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect archive path %q: %w", path, err)
		}
		path = filepath.Join(a.config.OutputDir, fmt.Sprintf("%s-%d.7z", base, suffix))
	}
}

func collectFiles(inputDir string) ([]sourceFile, error) {
	applog.Entry("archiver", "collectFiles", "inputDir=%s", inputDir)
	info, err := os.Stat(inputDir)
	if err != nil {
		return nil, fmt.Errorf("open archive input directory %q: %w", inputDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("archive input path %q is not a directory", inputDir)
	}

	var files []sourceFile
	err = filepath.WalkDir(inputDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file %q", path)
		}
		relativePath, err := filepath.Rel(inputDir, path)
		if err != nil {
			return err
		}
		files = append(files, sourceFile{
			relativePath: relativePath,
			size:         info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan archive input directory %q: %w", inputDir, err)
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].relativePath < files[j].relativePath
	})
	return files, nil
}

func groupFiles(files []sourceFile, maxSize int64) [][]sourceFile {
	applog.Entry("archiver", "groupFiles", "files=%d maxSize=%d", len(files), maxSize)
	var groups [][]sourceFile
	var current []sourceFile
	var currentSize int64
	for _, file := range files {
		if len(current) > 0 && currentSize+file.size > maxSize {
			groups = append(groups, current)
			current = nil
			currentSize = 0
		}
		current = append(current, file)
		currentSize += file.size
		if file.size > maxSize {
			groups = append(groups, current)
			current = nil
			currentSize = 0
		}
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

func clearDirectory(root string) error {
	applog.Entry("archiver", "clearDirectory", "root=%s", root)
	var directories []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root {
				directories = append(directories, path)
			}
			return nil
		}
		return os.Remove(path)
	})
	if err != nil {
		return err
	}
	sort.Slice(directories, func(i, j int) bool {
		return len(directories[i]) > len(directories[j])
	})
	for _, directory := range directories {
		if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func findSevenZip() (string, error) {
	applog.Entry("archiver", "findSevenZip", "start")
	for _, name := range []string{"7z", "7zz", "7za"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("7-Zip not found in PATH (expected 7z, 7zz, or 7za)")
}

func sanitizeName(value string) string {
	applog.Entry("archiver", "sanitizeName", "value=%s", value)
	value = strings.Map(func(character rune) rune {
		switch {
		case unicode.IsLetter(character), unicode.IsDigit(character):
			return character
		case character == '-', character == '_':
			return character
		default:
			return '_'
		}
	}, value)
	value = strings.Trim(value, "._- ")
	if value == "" {
		return "archive"
	}
	return value
}

func pathWithin(parent, child string) bool {
	applog.Entry("archiver", "pathWithin", "parent=%s child=%s", parent, child)
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative != ".." &&
		relative != "." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePath(left, right string) bool {
	applog.Entry("archiver", "samePath", "left=%s right=%s", left, right)
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
