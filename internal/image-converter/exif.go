package imageconverter

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

var (
	jpegSOI    = []byte{0xff, 0xd8}
	exifHeader = []byte("Exif\x00\x00")
)

// preserveEXIF copies EXIF APP1 segments from a JPEG source into the newly
// encoded JPEG. image/jpeg intentionally does not preserve metadata itself.
func preserveEXIF(sourcePath, outputPath string) error {
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	segments, err := extractEXIFSegments(source)
	if err != nil {
		// A decodable JPEG may still have malformed metadata. The converted
		// image is useful, so malformed EXIF is treated as absent.
		return nil
	}
	if len(segments) == 0 {
		return nil
	}

	output, err := os.ReadFile(outputPath)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(output, jpegSOI) {
		return errors.New("encoded output is not a JPEG")
	}

	extraLength := 0
	for _, segment := range segments {
		extraLength += len(segment)
	}
	withEXIF := make([]byte, 0, len(output)+extraLength)
	withEXIF = append(withEXIF, jpegSOI...)
	for _, segment := range segments {
		withEXIF = append(withEXIF, segment...)
	}
	withEXIF = append(withEXIF, output[len(jpegSOI):]...)

	if err := os.WriteFile(outputPath, withEXIF, 0o600); err != nil {
		return fmt.Errorf("write JPEG metadata: %w", err)
	}
	return nil
}

func extractEXIFSegments(data []byte) ([][]byte, error) {
	if !bytes.HasPrefix(data, jpegSOI) {
		return nil, nil
	}

	var result [][]byte
	for offset := len(jpegSOI); offset < len(data); {
		if data[offset] != 0xff {
			return nil, errors.New("invalid JPEG marker")
		}
		markerStart := offset
		for offset < len(data) && data[offset] == 0xff {
			offset++
		}
		if offset >= len(data) {
			return nil, errors.New("truncated JPEG marker")
		}

		marker := data[offset]
		offset++
		switch marker {
		case 0xd9, 0xda: // EOI or start of scan
			return result, nil
		case 0x01, 0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7:
			continue
		}
		if offset+2 > len(data) {
			return nil, errors.New("truncated JPEG segment length")
		}
		segmentLength := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		if segmentLength < 2 || offset+segmentLength > len(data) {
			return nil, errors.New("invalid JPEG segment length")
		}
		segmentEnd := offset + segmentLength
		payload := data[offset+2 : segmentEnd]
		if marker == 0xe1 && bytes.HasPrefix(payload, exifHeader) {
			segment := append([]byte(nil), data[markerStart:segmentEnd]...)
			result = append(result, segment)
		}
		offset = segmentEnd
	}
	return result, nil
}
