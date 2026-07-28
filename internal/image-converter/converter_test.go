package imageconverter

import (
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessDirResizesWithoutUpscalingAndSkipsBrokenFiles(t *testing.T) {
	root := t.TempDir()
	inputDir := filepath.Join(root, "input")
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(filepath.Join(inputDir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}

	largePath := filepath.Join(inputDir, "large.jpg")
	smallPath := filepath.Join(inputDir, "nested", "small.png")
	writeJPEG(t, largePath, image.NewRGBA(image.Rect(0, 0, 300, 150)))
	writePNG(t, smallPath, image.NewRGBA(image.Rect(0, 0, 40, 20)))
	if err := os.WriteFile(filepath.Join(inputDir, "broken.jpg"), []byte("not an image"), 0o644); err != nil {
		t.Fatal(err)
	}
	largeBefore, err := os.ReadFile(largePath)
	if err != nil {
		t.Fatal(err)
	}

	converter, err := New(Config{
		InputDir:     inputDir,
		OutputDir:    outputDir,
		MaxDimension: 100,
		Quality:      83,
	})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := converter.ProcessDir(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary != (Summary{Total: 3, Converted: 2, Failed: 1}) {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	assertDimensions(t, filepath.Join(outputDir, "large.jpg"), 100, 50)
	assertDimensions(t, filepath.Join(outputDir, "nested", "small.jpg"), 40, 20)

	largeAfter, err := os.ReadFile(largePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(largeAfter) != string(largeBefore) {
		t.Fatal("source image was modified")
	}
}

func TestProcessDirSkipsExistingOutput(t *testing.T) {
	root := t.TempDir()
	inputDir := filepath.Join(root, "input")
	outputDir := filepath.Join(root, "output")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJPEG(t, filepath.Join(inputDir, "photo.jpg"), image.NewRGBA(image.Rect(0, 0, 20, 10)))

	converter, err := New(Config{
		InputDir:     inputDir,
		OutputDir:    outputDir,
		MaxDimension: 100,
		Quality:      83,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := converter.ProcessDir(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	summary, err := converter.ProcessDir(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if summary != (Summary{Total: 1, Skipped: 1}) {
		t.Fatalf("unexpected second-run summary: %+v", summary)
	}
}

func writeJPEG(t *testing.T, path string, img image.Image) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(file, img, nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writePNG(t *testing.T, path string, img image.Image) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		t.Fatal(err)
	}
}

func assertDimensions(t *testing.T, path string, width, height int) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	actual, err := jpeg.DecodeConfig(file)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Width != width || actual.Height != height {
		t.Fatalf("dimensions of %s: got %dx%d, want %dx%d", path, actual.Width, actual.Height, width, height)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []Config{
		{OutputDir: "out", MaxDimension: 100, Quality: 83},
		{InputDir: "in", MaxDimension: 100, Quality: 83},
		{InputDir: "in", OutputDir: "out", Quality: 83},
		{InputDir: "in", OutputDir: "out", MaxDimension: 100, Quality: 101},
	}
	for _, config := range tests {
		if _, err := New(config); err == nil {
			t.Errorf("New(%+v) succeeded, want error", config)
		}
	}
}
