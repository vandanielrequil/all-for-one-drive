package oauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"all-for-one-drive/internal/applog"
)

const (
	tokenStartMarker = "Paste the following into your remote machine --->"
	tokenEndMarker   = "<---End paste"
)

// EnsureRcloneToken returns a persisted rclone OAuth token. If it does not
// exist yet, rclone opens the provider login page and receives the callback
// on localhost.
func EnsureRcloneToken(
	ctx context.Context,
	rcloneBinary string,
	backend string,
	clientID string,
	clientSecret string,
	tokenFile string,
) (string, error) {
	applog.Entry(
		"oauth",
		"EnsureRcloneToken",
		"backend=%s tokenFile=%s",
		backend,
		tokenFile,
	)
	token, err := readToken(tokenFile)
	if err == nil {
		applog.Entry("oauth", "EnsureRcloneToken", "using persisted token")
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if (clientID == "") != (clientSecret == "") {
		return "", errors.New("client_id and client_secret must be specified together")
	}

	args := []string{"authorize", backend}
	if clientID != "" {
		args = append(args, clientID, clientSecret)
	}
	applog.Entry("oauth", "EnsureRcloneToken", "starting authorization")
	cmd := exec.CommandContext(ctx, rcloneBinary, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = applog.Writer()
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("authorize rclone backend %q: %w", backend, err)
	}
	token, err = extractToken(stdout.String())
	if err != nil {
		return "", err
	}
	if err := SaveToken(tokenFile, token); err != nil {
		return "", err
	}
	applog.Entry("oauth", "EnsureRcloneToken", "authorization token persisted")
	return token, nil
}

// SaveToken atomically stores an OAuth token with user-only permissions.
func SaveToken(path string, token string) error {
	applog.Entry("oauth", "SaveToken", "path=%s", path)
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("OAuth token is empty")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create OAuth token directory: %w", err)
	}
	tempFile, err := os.CreateTemp(filepath.Dir(path), ".oauth-token-*")
	if err != nil {
		return fmt.Errorf("create temporary OAuth token file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if err := tempFile.Chmod(0o600); err != nil {
		tempFile.Close()
		return fmt.Errorf("protect temporary OAuth token file: %w", err)
	}
	if _, err := tempFile.WriteString(token); err != nil {
		tempFile.Close()
		return fmt.Errorf("write OAuth token: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close OAuth token file: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace OAuth token file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("persist OAuth token: %w", err)
	}
	return nil
}

func readToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("OAuth token file is empty")
	}
	return token, nil
}

func extractToken(output string) (string, error) {
	start := strings.Index(output, tokenStartMarker)
	if start < 0 {
		return "", errors.New("rclone authorization output does not contain token start marker")
	}
	start += len(tokenStartMarker)
	end := strings.Index(output[start:], tokenEndMarker)
	if end < 0 {
		return "", errors.New("rclone authorization output does not contain token end marker")
	}
	token := strings.TrimSpace(output[start : start+end])
	if token == "" {
		return "", errors.New("rclone authorization returned an empty token")
	}
	return token, nil
}
