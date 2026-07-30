package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"all-for-one-drive/internal/applog"
	"all-for-one-drive/internal/archiver"
	imageconverter "all-for-one-drive/internal/image-converter"
	rcloneclient "all-for-one-drive/internal/rclone"
	videoconverter "all-for-one-drive/internal/video-converter"
)

type Config struct {
	Replication    int                   `json:"replication"`
	Echelon        int                   `json:"echelon"`
	Archive        bool                  `json:"archive"`
	ImageConverter imageconverter.Config `json:"imageConverter"`
	VideoConverter videoconverter.Config `json:"videoConverter"`
	Archiver       archiver.Config       `json:"archiver"`
	Rclone         rcloneclient.Config   `json:"rclone"`
}

// Load reads a JSONC config and resolves module directory paths relative to it.
func Load(path string) (Config, error) {
	applog.Entry("config", "Load", "path=%s", path)
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	clean, err := stripComments(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse JSONC comments: %w", err)
	}

	var result Config
	decoder := json.NewDecoder(bytes.NewReader(clean))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return Config{}, err
	}
	if result.Replication == 0 {
		result.Replication = 3
	}
	if result.Echelon == 0 {
		result.Echelon = 1
	}
	if result.Replication < 1 || result.Replication > 3 {
		return Config{}, errors.New("replication must be between 1 and 3")
	}
	if result.Echelon < 1 || result.Echelon > 3 {
		return Config{}, errors.New("echelon must be between 1 and 3")
	}

	configPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config path: %w", err)
	}
	baseDir := filepath.Dir(configPath)
	result.ImageConverter.InputDir = resolvePath(baseDir, result.ImageConverter.InputDir)
	result.ImageConverter.OutputDir = resolvePath(baseDir, result.ImageConverter.OutputDir)
	result.VideoConverter.InputDir = resolvePath(baseDir, result.VideoConverter.InputDir)
	result.VideoConverter.OutputDir = resolvePath(baseDir, result.VideoConverter.OutputDir)
	result.Archiver.OutputDir = resolvePath(baseDir, result.Archiver.OutputDir)
	result.Rclone.UploadDir = resolvePath(baseDir, result.Rclone.UploadDir)
	for providerIndex := range result.Rclone.Providers {
		for accountIndex := range result.Rclone.Providers[providerIndex].Accounts {
			account := &result.Rclone.Providers[providerIndex].Accounts[accountIndex]
			if account.OAuth != nil {
				account.OAuth.TokenFile = resolvePath(baseDir, account.OAuth.TokenFile)
			}
			if account.TwoFactor != nil {
				account.TwoFactor.OutputFile = resolvePath(
					baseDir,
					account.TwoFactor.OutputFile,
				)
			}
		}
	}
	return result, nil
}

func ensureEOF(decoder *json.Decoder) error {
	applog.Entry("config", "ensureEOF", "start")
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode trailing config data: %w", err)
	}
	return errors.New("config contains multiple JSON values")
}

func resolvePath(baseDir, path string) string {
	applog.Entry("config", "resolvePath", "baseDir=%s path=%s", baseDir, path)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func stripComments(input []byte) ([]byte, error) {
	applog.Entry("config", "stripComments", "bytes=%d", len(input))
	output := make([]byte, 0, len(input))
	inString := false
	escaped := false

	for i := 0; i < len(input); {
		current := input[i]
		if inString {
			output = append(output, current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			i++
			continue
		}

		if current == '"' {
			inString = true
			output = append(output, current)
			i++
			continue
		}
		if current == '/' && i+1 < len(input) {
			switch input[i+1] {
			case '/':
				output = append(output, ' ', ' ')
				i += 2
				for i < len(input) && input[i] != '\n' {
					output = append(output, ' ')
					i++
				}
				continue
			case '*':
				output = append(output, ' ', ' ')
				i += 2
				closed := false
				for i < len(input) {
					if input[i] == '*' && i+1 < len(input) && input[i+1] == '/' {
						output = append(output, ' ', ' ')
						i += 2
						closed = true
						break
					}
					if input[i] == '\n' || input[i] == '\r' {
						output = append(output, input[i])
					} else {
						output = append(output, ' ')
					}
					i++
				}
				if !closed {
					return nil, errors.New("unterminated block comment")
				}
				continue
			}
		}

		output = append(output, current)
		i++
	}
	if inString {
		return nil, errors.New("unterminated string")
	}
	return output, nil
}
