package imageconverter

import (
	"encoding/binary"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

func TestPreserveEXIF(t *testing.T) {
	root := t.TempDir()
	sourcePath := filepath.Join(root, "source.jpg")
	outputPath := filepath.Join(root, "output.jpg")
	writeJPEG(t, sourcePath, image.NewRGBA(image.Rect(0, 0, 10, 10)))
	writeJPEG(t, outputPath, image.NewRGBA(image.Rect(0, 0, 5, 5)))

	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte("Exif\x00\x00"), []byte("test-metadata")...)
	segment := make([]byte, 4+len(payload))
	segment[0], segment[1] = 0xff, 0xe1
	binary.BigEndian.PutUint16(segment[2:4], uint16(len(payload)+2))
	copy(segment[4:], payload)
	source = append(append(append([]byte{}, source[:2]...), segment...), source[2:]...)
	if err := os.WriteFile(sourcePath, source, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := preserveEXIF(sourcePath, outputPath); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := extractEXIFSegments(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || string(segments[0]) != string(segment) {
		t.Fatalf("EXIF segment was not preserved: %x", segments)
	}

	file, err := os.Open(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := jpeg.Decode(file); err != nil {
		t.Fatalf("output JPEG is invalid: %v", err)
	}
}
