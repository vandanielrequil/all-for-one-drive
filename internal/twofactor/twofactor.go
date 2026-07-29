package twofactor

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"all-for-one-drive/internal/applog"
)

type Config struct {
	Secret       string `json:"secret"`
	OutputFile   string `json:"outputFile"`
	RcloneOption string `json:"rcloneOption"`
}

// Generate creates a six-digit RFC 6238 TOTP code valid for 30 seconds.
func Generate(secret string, now time.Time) (string, error) {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(secret), " ", ""))
	normalized = strings.TrimRight(normalized, "=")
	if normalized == "" {
		return "", errors.New("TOTP secret is empty")
	}
	decoded, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(normalized)
	if err != nil {
		return "", fmt.Errorf("decode Base32 TOTP secret: %w", err)
	}

	counter := uint64(now.Unix() / 30)
	var message [8]byte
	binary.BigEndian.PutUint64(message[:], counter)
	hash := hmac.New(sha1.New, decoded)
	if _, err := hash.Write(message[:]); err != nil {
		return "", fmt.Errorf("calculate TOTP: %w", err)
	}
	sum := hash.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset])&0x7f)<<24 |
		uint32(sum[offset+1])<<16 |
		uint32(sum[offset+2])<<8 |
		uint32(sum[offset+3])
	code := strconv.FormatUint(uint64(value%1_000_000), 10)
	return strings.Repeat("0", 6-len(code)) + code, nil
}

// GenerateAndWrite generates a fresh code and writes it to the configured file.
func GenerateAndWrite(config Config) (string, error) {
	applog.Entry(
		"twofactor",
		"GenerateAndWrite",
		"outputFile=%s rcloneOption=%s",
		config.OutputFile,
		config.RcloneOption,
	)
	if strings.TrimSpace(config.OutputFile) == "" {
		return "", errors.New("TOTP outputFile is required")
	}
	code, err := Generate(config.Secret, time.Now())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(config.OutputFile), 0o700); err != nil {
		return "", fmt.Errorf("create TOTP output directory: %w", err)
	}
	if err := os.WriteFile(config.OutputFile, []byte(code), 0o600); err != nil {
		return "", fmt.Errorf("write TOTP code: %w", err)
	}
	applog.Entry("twofactor", "GenerateAndWrite", "code written")
	return code, nil
}
