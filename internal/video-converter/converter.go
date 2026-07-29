package videoconverter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"all-for-one-drive/internal/applog"
)

const (
	DefaultMaxDimension = 2560
	DefaultQuality      = 83
)

// Config controls a single directory conversion run.
type Config struct {
	InputDir     string `json:"inputDir"`
	OutputDir    string `json:"outputDir"`
	MaxDimension int    `json:"maxDimension"`
	Quality      int    `json:"quality"`
}

// Progress describes the result of processing one source file.
type Progress struct {
	Current    int
	Total      int
	SourcePath string
	OutputPath string
	Skipped    bool
	Err        error
}

// Summary contains aggregate conversion results.
type Summary struct {
	Total     int
	Converted int
	Skipped   int
	Failed    int
}

// Converter converts supported videos without modifying source files.
type Converter struct {
	config  Config
	ffmpeg  string
	ffprobe string
}

func New(config Config) (*Converter, error) {
	applog.Entry("video-converter", "New", "inputDir=%s outputDir=%s maxDimension=%d quality=%d", config.InputDir, config.OutputDir, config.MaxDimension, config.Quality)
	if strings.TrimSpace(config.InputDir) == "" {
		return nil, errors.New("inputDir is required")
	}
	if strings.TrimSpace(config.OutputDir) == "" {
		return nil, errors.New("outputDir is required")
	}
	if config.MaxDimension <= 0 {
		return nil, errors.New("maxDimension must be greater than zero")
	}
	if config.Quality < 1 || config.Quality > 100 {
		return nil, errors.New("quality must be between 1 and 100")
	}

	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("ffmpeg not found in PATH")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, errors.New("ffprobe not found in PATH")
	}

	input, err := filepath.Abs(config.InputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve input directory: %w", err)
	}
	output, err := filepath.Abs(config.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory: %w", err)
	}
	if samePath(input, output) {
		return nil, errors.New("inputDir and outputDir must be different")
	}
	config.InputDir = input
	config.OutputDir = output

	return &Converter{config: config, ffmpeg: ffmpeg, ffprobe: ffprobe}, nil
}

// ProcessDir recursively converts supported videos and reports each result.
// Existing output files are skipped, which makes repeated runs non-destructive.
func (c *Converter) ProcessDir(ctx context.Context, report func(Progress)) (Summary, error) {
	applog.Entry("video-converter", "ProcessDir", "inputDir=%s outputDir=%s", c.config.InputDir, c.config.OutputDir)
	if err := os.MkdirAll(c.config.OutputDir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("create video output directory: %w", err)
	}
	files, err := c.sourceFiles()
	if err != nil {
		return Summary{}, err
	}

	summary := Summary{Total: len(files)}
	for index, sourcePath := range files {
		if err := ctx.Err(); err != nil {
			return summary, err
		}

		outputPath, skipped, convertErr := c.convertFile(ctx, sourcePath)
		progress := Progress{
			Current:    index + 1,
			Total:      len(files),
			SourcePath: sourcePath,
			OutputPath: outputPath,
			Skipped:    skipped,
			Err:        convertErr,
		}
		switch {
		case convertErr != nil:
			summary.Failed++
		case skipped:
			summary.Skipped++
		default:
			summary.Converted++
		}
		if report != nil {
			report(progress)
		}
	}

	return summary, nil
}

func (c *Converter) sourceFiles() ([]string, error) {
	applog.Entry("video-converter", "sourceFiles", "inputDir=%s", c.config.InputDir)
	info, err := os.Stat(c.config.InputDir)
	if err != nil {
		return nil, fmt.Errorf("open input directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("input path %q is not a directory", c.config.InputDir)
	}

	var files []string
	err = filepath.WalkDir(c.config.InputDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if samePath(path, c.config.OutputDir) {
				return filepath.SkipDir
			}
			return nil
		}
		if isSupportedVideo(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan input directory: %w", err)
	}
	return files, nil
}

func (c *Converter) convertFile(ctx context.Context, sourcePath string) (outputPath string, skipped bool, err error) {
	applog.Entry("video-converter", "convertFile", "sourcePath=%s", sourcePath)
	relativePath, err := filepath.Rel(c.config.InputDir, sourcePath)
	if err != nil {
		return "", false, fmt.Errorf("resolve relative path: %w", err)
	}
	outputPath = filepath.Join(
		c.config.OutputDir,
		strings.TrimSuffix(relativePath, filepath.Ext(relativePath))+".mp4",
	)
	if _, err := os.Stat(outputPath); err == nil {
		return outputPath, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return outputPath, false, fmt.Errorf("inspect output file: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return outputPath, false, fmt.Errorf("create output directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".video-converter-*.mp4")
	if err != nil {
		return outputPath, false, fmt.Errorf("create temporary output: %w", err)
	}
	tempPath := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(tempPath)
		return outputPath, false, fmt.Errorf("close temporary output: %w", err)
	}
	defer os.Remove(tempPath)

	if err := c.runFFmpeg(ctx, sourcePath, tempPath); err != nil {
		return outputPath, false, err
	}
	if err := os.Rename(tempPath, outputPath); err != nil {
		return outputPath, false, fmt.Errorf("publish output: %w", err)
	}
	return outputPath, false, nil
}

func (c *Converter) runFFmpeg(ctx context.Context, sourcePath, outputPath string) error {
	applog.Entry("video-converter", "runFFmpeg", "sourcePath=%s outputPath=%s", sourcePath, outputPath)
	width, height, err := c.probeVideoSize(ctx, sourcePath)
	if err != nil {
		return fmt.Errorf("probe video: %w", err)
	}

	args := []string{
		"-loglevel", "info",
		"-stats",
		"-nostdin",
		"-y",
		"-i", sourcePath,
	}
	if width > c.config.MaxDimension || height > c.config.MaxDimension {
		var scale string
		if width >= height {
			scale = fmt.Sprintf("scale=%d:-2", c.config.MaxDimension)
		} else {
			scale = fmt.Sprintf("scale=-2:%d", c.config.MaxDimension)
		}
		args = append(args, "-vf", scale)
		fmt.Fprintf(os.Stderr, "resize: %dx%d -> max side %d\n", width, height, c.config.MaxDimension)
	} else {
		fmt.Fprintf(os.Stderr, "resize: skip (%dx%d)\n", width, height)
	}
	args = append(args,
		"-map", "0:v:0",
		"-map", "0:a?",
		"-c:v", "libx264",
		"-crf", fmt.Sprintf("%d", qualityToCRF(c.config.Quality)),
		"-preset", "medium",
		"-c:a", "aac",
		"-b:a", "128k",
		"-map_metadata", "0",
		"-movflags", "+faststart",
		outputPath,
	)

	fmt.Fprintf(os.Stderr, "ffmpeg: %s -> %s\n", sourcePath, outputPath)

	cmd := exec.CommandContext(ctx, c.ffmpeg, args...)
	cmd.Stdout = applog.Writer()
	cmd.Stderr = applog.Writer()
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg failed: %w", err)
	}
	return nil
}

func (c *Converter) probeVideoSize(ctx context.Context, sourcePath string) (int, int, error) {
	applog.Entry("video-converter", "probeVideoSize", "sourcePath=%s", sourcePath)
	cmd := exec.CommandContext(ctx, c.ffprobe,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0:s=x",
		sourcePath,
	)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, applog.Writer())
	cmd.Stderr = io.MultiWriter(&stderr, applog.Writer())
	err := cmd.Run()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	output := stdout.Bytes()
	parts := strings.Split(strings.TrimSpace(string(output)), "x")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected ffprobe output: %q", output)
	}
	width, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, err
	}
	height, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return width, height, nil
}

// qualityToCRF maps config quality (1-100) to libx264 CRF (lower is better).
func qualityToCRF(quality int) int {
	applog.Entry("video-converter", "qualityToCRF", "quality=%d", quality)
	crf := 51 - int(float64(quality)*0.37)
	if crf < 0 {
		return 0
	}
	if crf > 51 {
		return 51
	}
	return crf
}

func isSupportedVideo(path string) bool {
	applog.Entry("video-converter", "isSupportedVideo", "path=%s", path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".mov", ".avi", ".mkv", ".webm", ".m4v", ".wmv", ".flv", ".mpeg", ".mpg", ".3gp":
		return true
	default:
		return false
	}
}

func samePath(left, right string) bool {
	applog.Entry("video-converter", "samePath", "left=%s right=%s", left, right)
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
