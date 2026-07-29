package imageconverter

import (
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"all-for-one-drive/internal/applog"
	"github.com/disintegration/imaging"
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

// Converter converts supported images without modifying source files.
type Converter struct {
	config Config
}

func New(config Config) (*Converter, error) {
	applog.Entry("image-converter", "New", "inputDir=%s outputDir=%s maxDimension=%d quality=%d", config.InputDir, config.OutputDir, config.MaxDimension, config.Quality)
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

	return &Converter{config: config}, nil
}

// ProcessDir recursively converts supported images and reports each result.
// Existing output files are skipped, which makes repeated runs non-destructive.
func (c *Converter) ProcessDir(ctx context.Context, report func(Progress)) (Summary, error) {
	applog.Entry("image-converter", "ProcessDir", "inputDir=%s outputDir=%s", c.config.InputDir, c.config.OutputDir)
	if err := os.MkdirAll(c.config.OutputDir, 0o755); err != nil {
		return Summary{}, fmt.Errorf("create image output directory: %w", err)
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

		outputPath, skipped, convertErr := c.convertFile(sourcePath)
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
	applog.Entry("image-converter", "sourceFiles", "inputDir=%s", c.config.InputDir)
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
		if isSupportedImage(path) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan input directory: %w", err)
	}
	return files, nil
}

func (c *Converter) convertFile(sourcePath string) (outputPath string, skipped bool, err error) {
	applog.Entry("image-converter", "convertFile", "sourcePath=%s", sourcePath)
	relativePath, err := filepath.Rel(c.config.InputDir, sourcePath)
	if err != nil {
		return "", false, fmt.Errorf("resolve relative path: %w", err)
	}
	outputPath = filepath.Join(
		c.config.OutputDir,
		strings.TrimSuffix(relativePath, filepath.Ext(relativePath))+".jpg",
	)
	if _, err := os.Stat(outputPath); err == nil {
		return outputPath, true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return outputPath, false, fmt.Errorf("inspect output file: %w", err)
	}

	img, err := imaging.Open(sourcePath)
	if err != nil {
		return outputPath, false, fmt.Errorf("decode image: %w", err)
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width > c.config.MaxDimension || height > c.config.MaxDimension {
		if width >= height {
			img = imaging.Resize(img, c.config.MaxDimension, 0, imaging.Lanczos)
		} else {
			img = imaging.Resize(img, 0, c.config.MaxDimension, imaging.Lanczos)
		}
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return outputPath, false, fmt.Errorf("create output directory: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(outputPath), ".image-converter-*.jpg")
	if err != nil {
		return outputPath, false, fmt.Errorf("create temporary output: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)

	if err := jpeg.Encode(temp, img, &jpeg.Options{Quality: c.config.Quality}); err != nil {
		temp.Close()
		return outputPath, false, fmt.Errorf("encode JPEG: %w", err)
	}
	if err := temp.Close(); err != nil {
		return outputPath, false, fmt.Errorf("close temporary output: %w", err)
	}
	if err := preserveEXIF(sourcePath, tempPath); err != nil {
		return outputPath, false, fmt.Errorf("preserve EXIF: %w", err)
	}
	if err := os.Rename(tempPath, outputPath); err != nil {
		return outputPath, false, fmt.Errorf("publish output: %w", err)
	}
	return outputPath, false, nil
}

func isSupportedImage(path string) bool {
	applog.Entry("image-converter", "isSupportedImage", "path=%s", path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".tif", ".tiff", ".bmp":
		return true
	default:
		return false
	}
}

func samePath(left, right string) bool {
	applog.Entry("image-converter", "samePath", "left=%s right=%s", left, right)
	return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}
