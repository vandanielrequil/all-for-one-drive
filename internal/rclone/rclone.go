package rclone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"all-for-one-drive/internal/applog"
)

type Config struct {
	Providers []Provider `json:"providers"`
	UploadDir string     `json:"uploadDir"`
}

type Provider struct {
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Priority        int       `json:"priority"`
	AcceptsArchives bool      `json:"acceptsArchives"`
	Accounts        []Account `json:"accounts"`
}

type Account struct {
	Name     string            `json:"name"`
	RootPath string            `json:"rootPath"`
	Options  map[string]string `json:"options"`
}

type Target struct {
	Provider string
	Account  string
}

type CheckResult struct {
	Provider    string
	Account     string
	BackendType string
	Priority    int
	LoginOK     bool
	FreeBytes   *int64
	Usage       string
	Error       string
}

type Client struct {
	binary     string
	configPath string
	remotes    map[Target]remote
}

type remote struct {
	name        string
	backendType string
	rootPath    string
	priority    int
	archives    bool
	cloudName   string
	apiKey      string
	apiSecret   string
}

type uploadFile struct {
	localPath    string
	relativePath string
}

type quota struct {
	Free *int64 `json:"free"`
}

type cloudinaryUsage struct {
	Credits struct {
		Usage       float64 `json:"usage"`
		Limit       float64 `json:"limit"`
		UsedPercent float64 `json:"used_percent"`
	} `json:"credits"`
}

func New(config Config) (*Client, error) {
	applog.Entry("rclone", "New", "providers=%d", len(config.Providers))
	if len(config.Providers) == 0 {
		return nil, errors.New("providers must not be empty")
	}
	if strings.TrimSpace(config.UploadDir) == "" {
		return nil, errors.New("uploadDir is required")
	}

	binary, err := exec.LookPath("rclone")
	if err != nil {
		return nil, errors.New("rclone not found in PATH")
	}
	remotes, _, err := buildConfig(config)
	if err != nil {
		return nil, err
	}
	configFile, err := os.CreateTemp("", "all-for-one-rclone-*.conf")
	if err != nil {
		return nil, fmt.Errorf("create temporary rclone config: %w", err)
	}
	configPath := configFile.Name()
	if err := configFile.Chmod(0o600); err != nil {
		configFile.Close()
		os.Remove(configPath)
		return nil, fmt.Errorf("protect temporary rclone config: %w", err)
	}
	if err := configFile.Close(); err != nil {
		os.Remove(configPath)
		return nil, fmt.Errorf("close temporary rclone config: %w", err)
	}
	if err := createRcloneConfig(binary, configPath, config); err != nil {
		os.Remove(configPath)
		return nil, err
	}
	generatedConfig, err := os.ReadFile(configPath)
	if err != nil {
		os.Remove(configPath)
		return nil, fmt.Errorf("read generated rclone config: %w", err)
	}
	applog.Entry(
		"rclone",
		"New",
		"generatedConfig:\n%s",
		redactConfigContent(generatedConfig),
	)

	applog.Entry(
		"rclone",
		"New",
		"binary=%s configPath=%s remotes=%d",
		binary,
		configPath,
		len(remotes),
	)
	return &Client{
		binary:     binary,
		configPath: configPath,
		remotes:    remotes,
	}, nil
}

func createRcloneConfig(binary, configPath string, config Config) error {
	applog.Entry(
		"rclone",
		"createRcloneConfig",
		"binary=%s configPath=%s providers=%d",
		binary,
		configPath,
		len(config.Providers),
	)
	for providerIndex, provider := range config.Providers {
		for accountIndex, account := range provider.Accounts {
			remoteName := fmt.Sprintf(
				"afo_%d_%d_%s",
				providerIndex,
				accountIndex,
				sanitizeRemoteName(account.Name),
			)
			optionKeys := make([]string, 0, len(account.Options))
			for key := range account.Options {
				optionKeys = append(optionKeys, key)
			}
			sort.Strings(optionKeys)

			args := []string{
				"--config", configPath,
				"config", "create",
				remoteName,
				provider.Type,
			}
			for _, key := range optionKeys {
				args = append(args, key, account.Options[key])
			}
			args = append(args, "--obscure", "--no-output")

			applog.Entry(
				"rclone",
				"createRcloneConfig",
				"remote=%s type=%s optionKeys=%v",
				remoteName,
				provider.Type,
				optionKeys,
			)
			cmd := exec.Command(binary, args...)
			cmd.Stdout = applog.Writer()
			cmd.Stderr = applog.Writer()
			if err := cmd.Run(); err != nil {
				return fmt.Errorf(
					"create rclone remote %q for provider %q account %q: %w",
					remoteName,
					provider.Name,
					account.Name,
					err,
				)
			}
		}
	}
	return nil
}

func (c *Client) Close() error {
	applog.Entry("rclone", "Close", "configPath=%s", c.configPath)
	if c.configPath == "" {
		return nil
	}
	err := os.Remove(c.configPath)
	c.configPath = ""
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove temporary rclone config: %w", err)
	}
	return nil
}

// PreCheck checks authentication and available space for every configured account.
func (c *Client) PreCheck(ctx context.Context) []CheckResult {
	applog.Entry("rclone", "PreCheck", "remotes=%d", len(c.remotes))
	targets := make([]Target, 0, len(c.remotes))
	for target := range c.remotes {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		left, right := c.remotes[targets[i]], c.remotes[targets[j]]
		if left.priority != right.priority {
			return left.priority > right.priority
		}
		if targets[i].Provider != targets[j].Provider {
			return targets[i].Provider < targets[j].Provider
		}
		return targets[i].Account < targets[j].Account
	})

	results := make([]CheckResult, 0, len(targets))
	for _, target := range targets {
		if ctx.Err() != nil {
			results = append(results, CheckResult{
				Provider:    target.Provider,
				Account:     target.Account,
				BackendType: c.remotes[target].backendType,
				Priority:    c.remotes[target].priority,
				Error:       ctx.Err().Error(),
			})
			continue
		}
		results = append(results, c.checkRemote(ctx, target))
	}
	logCheckTable(results)
	return results
}

// BestTarget returns the available account with the highest provider priority.
func (c *Client) BestTarget(backendType string, results []CheckResult) (Target, error) {
	applog.Entry(
		"rclone",
		"BestTarget",
		"backendType=%s results=%d",
		backendType,
		len(results),
	)
	var selected *CheckResult
	for index := range results {
		result := &results[index]
		if !result.LoginOK || !strings.EqualFold(result.BackendType, backendType) {
			continue
		}
		if selected == nil || result.Priority > selected.Priority {
			selected = result
		}
	}
	if selected == nil {
		return Target{}, fmt.Errorf("no available rclone account for backend %q", backendType)
	}
	target := Target{Provider: selected.Provider, Account: selected.Account}
	applog.Entry(
		"rclone",
		"BestTarget",
		"provider=%s account=%s priority=%d",
		target.Provider,
		target.Account,
		selected.Priority,
	)
	return target, nil
}

func (c *Client) AcceptsArchives(target Target) (bool, error) {
	applog.Entry(
		"rclone",
		"AcceptsArchives",
		"provider=%s account=%s",
		target.Provider,
		target.Account,
	)
	selected, ok := c.remotes[target]
	if !ok {
		return false, fmt.Errorf(
			"rclone target %q/%q is not configured",
			target.Provider,
			target.Account,
		)
	}
	return selected.archives, nil
}

// Upload copies files or directories to the selected provider account.
func (c *Client) Upload(
	ctx context.Context,
	target Target,
	sourcePaths []string,
	destination string,
) error {
	applog.Entry(
		"rclone",
		"Upload",
		"provider=%s account=%s sources=%v destination=%s",
		target.Provider,
		target.Account,
		sourcePaths,
		destination,
	)
	if len(sourcePaths) == 0 {
		return errors.New("sourcePaths must not be empty")
	}
	selected, ok := c.remotes[target]
	if !ok {
		return fmt.Errorf("rclone target %q/%q is not configured", target.Provider, target.Account)
	}

	remoteRoot := joinRemotePath(selected.rootPath, destination)
	remoteDirectory := selected.name + ":" + remoteRoot
	if err := c.runStreaming(
		ctx,
		"Upload.mkdir",
		"--config", c.configPath,
		"mkdir", remoteDirectory,
	); err != nil {
		return fmt.Errorf("create remote directory %q: %w", remoteDirectory, err)
	}

	files, err := collectUploadFiles(sourcePaths)
	if err != nil {
		return err
	}
	createdDirectories := map[string]struct{}{remoteDirectory: {}}
	for _, file := range files {
		relativeDirectory := filepath.Dir(file.relativePath)
		fileDirectory := remoteDirectory
		if relativeDirectory != "." {
			fileDirectory = selected.name + ":" + joinRemotePath(
				remoteRoot,
				relativeDirectory,
			)
			if _, exists := createdDirectories[fileDirectory]; !exists {
				if err := c.runStreaming(
					ctx,
					"Upload.mkdir",
					"--config", c.configPath,
					"mkdir", fileDirectory,
				); err != nil {
					return fmt.Errorf("create remote directory %q: %w", fileDirectory, err)
				}
				createdDirectories[fileDirectory] = struct{}{}
			}
		}
		args := []string{
			"--config", c.configPath,
			"--stats", "5s",
			"--stats-one-line",
			"copy",
			"--no-traverse",
			file.localPath,
			fileDirectory,
		}
		if err := c.runStreaming(ctx, "Upload", args...); err != nil {
			return fmt.Errorf("upload %q to %q: %w", file.localPath, fileDirectory, err)
		}
	}
	return nil
}

func collectUploadFiles(sourcePaths []string) ([]uploadFile, error) {
	applog.Entry("rclone", "collectUploadFiles", "sourcePaths=%v", sourcePaths)
	var files []uploadFile
	for _, sourcePath := range sourcePaths {
		info, err := os.Stat(sourcePath)
		if err != nil {
			return nil, fmt.Errorf("inspect upload source %q: %w", sourcePath, err)
		}
		if !info.IsDir() {
			files = append(files, uploadFile{
				localPath:    sourcePath,
				relativePath: filepath.Base(sourcePath),
			})
			continue
		}
		err = filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("unsupported non-regular upload file %q", path)
			}
			relativePath, err := filepath.Rel(sourcePath, path)
			if err != nil {
				return err
			}
			files = append(files, uploadFile{
				localPath:    path,
				relativePath: relativePath,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan upload source %q: %w", sourcePath, err)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].localPath < files[j].localPath
	})
	applog.Entry("rclone", "collectUploadFiles", "files=%d", len(files))
	return files, nil
}

func (c *Client) checkRemote(ctx context.Context, target Target) CheckResult {
	selected := c.remotes[target]
	applog.Entry(
		"rclone",
		"checkRemote",
		"provider=%s account=%s remote=%s priority=%d",
		target.Provider,
		target.Account,
		selected.name,
		selected.priority,
	)
	result := CheckResult{
		Provider:    target.Provider,
		Account:     target.Account,
		BackendType: selected.backendType,
		Priority:    selected.priority,
	}
	if strings.EqualFold(selected.backendType, "cloudinary") {
		usage, err := c.fetchCloudinaryUsage(ctx, selected)
		if err != nil {
			applog.Entry(
				"rclone",
				"checkRemote",
				"step=cloudinary-usage failed login=ERROR err=%v",
				err,
			)
			result.Error = compactError(err)
			return result
		}
		result.Usage = fmt.Sprintf(
			"%.2f / %.2f кредитов (%.1f%%)",
			usage.Credits.Usage,
			usage.Credits.Limit,
			usage.Credits.UsedPercent,
		)
		applog.Entry(
			"rclone",
			"checkRemote",
			"step=cloudinary-usage success usage=%s",
			result.Usage,
		)

		probePath := selected.name + ":" + joinRemotePath(selected.rootPath, "all-for-one")
		applog.Entry(
			"rclone",
			"checkRemote",
			"step=cloudinary-mkdir start remote=%s",
			probePath,
		)
		_, err = c.runCapture(ctx, "-vv", "mkdir", probePath)
		if err != nil {
			applog.Entry(
				"rclone",
				"checkRemote",
				"step=cloudinary-mkdir failed login=ERROR err=%v",
				err,
			)
			result.Error = compactError(err)
			return result
		}
		applog.Entry(
			"rclone",
			"checkRemote",
			"step=cloudinary-mkdir success login=OK freeSpace=unsupported",
		)
		result.LoginOK = true
		return result
	}

	// Check the remote root: configured upload folders may not exist yet.
	remotePath := selected.name + ":"
	applog.Entry("rclone", "checkRemote", "step=about start remote=%s", remotePath)
	output, err := c.runCapture(ctx, "-vv", "about", "--json", remotePath)
	if err == nil {
		applog.Entry("rclone", "checkRemote", "step=about success responseBytes=%d", len(output))
		var remoteQuota quota
		if jsonErr := json.Unmarshal(output, &remoteQuota); jsonErr != nil {
			applog.Entry("rclone", "checkRemote", "step=about decode-error err=%v", jsonErr)
			result.Error = "invalid quota response: " + jsonErr.Error()
			return result
		}
		result.LoginOK = true
		result.FreeBytes = remoteQuota.Free
		return result
	}
	applog.Entry("rclone", "checkRemote", "step=about failed err=%v fallback=lsd", err)

	// Some backends authenticate correctly but do not implement About.
	applog.Entry("rclone", "checkRemote", "step=lsd start remote=%s maxDepth=1", remotePath)
	_, listErr := c.runCapture(ctx, "-vv", "lsd", "--max-depth", "1", remotePath)
	if listErr == nil {
		applog.Entry("rclone", "checkRemote", "step=lsd success login=OK")
		result.LoginOK = true
		return result
	}
	applog.Entry("rclone", "checkRemote", "step=lsd failed login=ERROR err=%v", listErr)
	result.Error = compactError(listErr)
	return result
}

func (c *Client) fetchCloudinaryUsage(
	ctx context.Context,
	selected remote,
) (cloudinaryUsage, error) {
	applog.Entry(
		"rclone",
		"fetchCloudinaryUsage",
		"cloudName=%s apiKey=%s",
		selected.cloudName,
		maskCredential(selected.apiKey),
	)
	endpoint := fmt.Sprintf(
		"https://api.cloudinary.com/v1_1/%s/usage",
		selected.cloudName,
	)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return cloudinaryUsage{}, err
	}
	request.SetBasicAuth(selected.apiKey, selected.apiSecret)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return cloudinaryUsage{}, err
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return cloudinaryUsage{}, fmt.Errorf(
			"Cloudinary usage API returned %s: %s",
			response.Status,
			strings.TrimSpace(string(body)),
		)
	}
	var usage cloudinaryUsage
	if err := json.NewDecoder(response.Body).Decode(&usage); err != nil {
		return cloudinaryUsage{}, fmt.Errorf("decode Cloudinary usage: %w", err)
	}
	return usage, nil
}

func (c *Client) runCapture(ctx context.Context, args ...string) ([]byte, error) {
	applog.Entry("rclone", "runCapture", "args=%v", safeArgs(args))
	startedAt := time.Now()
	commandArgs := append([]string{"--config", c.configPath}, args...)
	cmd := exec.CommandContext(ctx, c.binary, commandArgs...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, applog.Writer())
	cmd.Stderr = io.MultiWriter(&stderr, applog.Writer())
	if err := cmd.Run(); err != nil {
		applog.Entry(
			"rclone",
			"runCapture",
			"result=failed elapsed=%s stdoutBytes=%d stderrBytes=%d err=%v",
			time.Since(startedAt).Round(time.Millisecond),
			stdout.Len(),
			stderr.Len(),
			err,
		)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	applog.Entry(
		"rclone",
		"runCapture",
		"result=success elapsed=%s stdoutBytes=%d stderrBytes=%d",
		time.Since(startedAt).Round(time.Millisecond),
		stdout.Len(),
		stderr.Len(),
	)
	return stdout.Bytes(), nil
}

func (c *Client) runStreaming(ctx context.Context, function string, args ...string) error {
	applog.Entry("rclone", "runStreaming", "function=%s args=%v", function, safeArgs(args))
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Stdout = applog.Writer()
	cmd.Stderr = applog.Writer()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

func buildConfig(config Config) (map[Target]remote, []byte, error) {
	applog.Entry("rclone", "buildConfig", "providers=%d", len(config.Providers))
	remotes := make(map[Target]remote)
	providerNames := make(map[string]struct{})
	var content strings.Builder

	for providerIndex, provider := range config.Providers {
		if strings.TrimSpace(provider.Name) == "" {
			return nil, nil, fmt.Errorf("providers[%d].name is required", providerIndex)
		}
		if strings.TrimSpace(provider.Type) == "" {
			return nil, nil, fmt.Errorf("provider %q type is required", provider.Name)
		}
		if provider.Priority < 1 || provider.Priority > 100 {
			return nil, nil, fmt.Errorf("provider %q priority must be between 1 and 100", provider.Name)
		}
		providerKey := strings.ToLower(provider.Name)
		if _, exists := providerNames[providerKey]; exists {
			return nil, nil, fmt.Errorf("duplicate provider name %q", provider.Name)
		}
		providerNames[providerKey] = struct{}{}

		accountNames := make(map[string]struct{})
		for accountIndex, account := range provider.Accounts {
			if strings.TrimSpace(account.Name) == "" {
				return nil, nil, fmt.Errorf(
					"provider %q accounts[%d].name is required",
					provider.Name,
					accountIndex,
				)
			}
			accountKey := strings.ToLower(account.Name)
			if _, exists := accountNames[accountKey]; exists {
				return nil, nil, fmt.Errorf(
					"provider %q has duplicate account %q",
					provider.Name,
					account.Name,
				)
			}
			accountNames[accountKey] = struct{}{}
			logCredentialSummary(provider, account)
			remoteName := fmt.Sprintf(
				"afo_%d_%d_%s",
				providerIndex,
				accountIndex,
				sanitizeRemoteName(account.Name),
			)
			target := Target{Provider: provider.Name, Account: account.Name}
			remotes[target] = remote{
				name:        remoteName,
				backendType: provider.Type,
				rootPath:    cleanRemotePath(account.RootPath),
				priority:    provider.Priority,
				archives:    provider.AcceptsArchives,
				cloudName:   account.Options["cloud_name"],
				apiKey:      account.Options["api_key"],
				apiSecret:   account.Options["api_secret"],
			}

			content.WriteString("[")
			content.WriteString(remoteName)
			content.WriteString("]\n")
			content.WriteString("type = ")
			content.WriteString(provider.Type)
			content.WriteString("\n")
			optionKeys := make([]string, 0, len(account.Options))
			for key := range account.Options {
				optionKeys = append(optionKeys, key)
			}
			sort.Strings(optionKeys)
			for _, key := range optionKeys {
				if err := validateConfigValue(key, account.Options[key]); err != nil {
					return nil, nil, fmt.Errorf(
						"provider %q account %q: %w",
						provider.Name,
						account.Name,
						err,
					)
				}
				content.WriteString(key)
				content.WriteString(" = ")
				content.WriteString(account.Options[key])
				content.WriteString("\n")
			}
			content.WriteString("\n")
		}
	}
	if len(remotes) == 0 {
		return nil, nil, errors.New("at least one rclone account is required")
	}
	return remotes, []byte(content.String()), nil
}

func logCredentialSummary(provider Provider, account Account) {
	applog.Entry(
		"rclone",
		"logCredentialSummary",
		"provider=%s type=%s account=%s cloud_name=%s api_key=%s api_secret=%s api_secret_length=%d",
		provider.Name,
		provider.Type,
		account.Name,
		account.Options["cloud_name"],
		account.Options["api_key"],
		maskCredential(account.Options["api_secret"]),
		len(account.Options["api_secret"]),
	)
}

func redactConfigContent(content []byte) string {
	applog.Entry("rclone", "redactConfigContent", "bytes=%d", len(content))
	lines := strings.Split(string(content), "\n")
	for index, line := range lines {
		key, _, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if isSensitiveConfigKey(key) {
			lines[index] = strings.TrimSpace(strings.SplitN(line, "=", 2)[0]) + " = ***"
		}
	}
	return strings.Join(lines, "\n")
}

func isSensitiveConfigKey(key string) bool {
	applog.Entry("rclone", "isSensitiveConfigKey", "key=%s", key)
	return key == "key" ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "password") ||
		strings.Contains(key, "pass") ||
		strings.Contains(key, "token")
}

func maskCredential(value string) string {
	applog.Entry("rclone", "maskCredential", "valueLength=%d", len(value))
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return strings.Repeat("*", len(value)-4) + value[len(value)-4:]
}

func validateConfigValue(key, value string) error {
	applog.Entry("rclone", "validateConfigValue", "key=%s valueLength=%d", key, len(value))
	if strings.TrimSpace(key) == "" {
		return errors.New("option key must not be empty")
	}
	if strings.ContainsAny(key, "=\r\n[]") {
		return fmt.Errorf("invalid option key %q", key)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("option %q must be a single line", key)
	}
	return nil
}

func logCheckTable(results []CheckResult) {
	applog.Entry("rclone", "logCheckTable", "results=%d", len(results))
	applog.Entry(
		"rclone",
		"PreCheck",
		"%-20s | %-20s | %-12s | %s",
		"ОБЛАКО",
		"АККАУНТ",
		"ЛОГИН",
		"КВОТА / КРЕДИТЫ",
	)
	for _, result := range results {
		status := "OK"
		if !result.LoginOK {
			status = "ОШИБКА"
		}
		free := "не поддерживается"
		if result.FreeBytes != nil {
			free = formatBytes(*result.FreeBytes)
		}
		if result.Usage != "" {
			free = result.Usage
		}
		if result.Error != "" {
			free = result.Error
		}
		applog.Entry(
			"rclone",
			"PreCheck",
			"%-20s | %-20s | %-12s | %s",
			result.Provider,
			result.Account,
			status,
			free,
		)
	}
}

func sanitizeRemoteName(value string) string {
	applog.Entry("rclone", "sanitizeRemoteName", "value=%s", value)
	value = strings.Map(func(character rune) rune {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' {
			return character
		}
		return '_'
	}, value)
	if value == "" {
		return "account"
	}
	return value
}

func cleanRemotePath(value string) string {
	applog.Entry("rclone", "cleanRemotePath", "value=%s", value)
	return strings.Trim(strings.ReplaceAll(value, "\\", "/"), "/")
}

func joinRemotePath(parts ...string) string {
	applog.Entry("rclone", "joinRemotePath", "parts=%v", parts)
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		if clean := cleanRemotePath(part); clean != "" {
			cleaned = append(cleaned, clean)
		}
	}
	return strings.Join(cleaned, "/")
}

func formatBytes(value int64) string {
	applog.Entry("rclone", "formatBytes", "value=%d", value)
	const unit = int64(1024)
	if value < unit {
		return strconv.FormatInt(value, 10) + " B"
	}
	divisor := unit
	exponent := 0
	for reduced := value / unit; reduced >= unit && exponent < 5; reduced /= unit {
		divisor *= unit
		exponent++
	}
	return fmt.Sprintf("%.2f %ciB", float64(value)/float64(divisor), "KMGTPE"[exponent])
}

func safeArgs(args []string) []string {
	applog.Entry("rclone", "safeArgs", "argsCount=%d", len(args))
	safe := append([]string(nil), args...)
	for index, argument := range safe {
		if strings.HasPrefix(argument, "--password-command") {
			safe[index] = "--password-command=***"
		}
	}
	return safe
}

func compactError(err error) string {
	applog.Entry("rclone", "compactError", "err=%v", err)
	value := strings.Join(strings.Fields(err.Error()), " ")
	if len(value) > 180 {
		return value[:177] + "..."
	}
	return value
}
