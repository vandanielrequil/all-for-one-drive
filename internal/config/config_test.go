package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadJSONCAndResolvePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "all-for-one.config.jsonc")
	content := `{
		// A line comment.
		"imageConverter": {
			"inputDir": "./photos", /* a block comment */
			"outputDir": "folder//literal",
			"maxDimension": 2560,
			"quality": 83
		}
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.ImageConverter.InputDir != filepath.Join(root, "photos") {
		t.Fatalf("unexpected input path: %s", result.ImageConverter.InputDir)
	}
	if result.ImageConverter.OutputDir != filepath.Join(root, "folder", "literal") {
		t.Fatalf("unexpected output path: %s", result.ImageConverter.OutputDir)
	}
}

func TestLoadRejectsUnterminatedComment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.jsonc")
	if err := os.WriteFile(path, []byte(`{"imageConverter": {/*`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded, want error")
	}
}
