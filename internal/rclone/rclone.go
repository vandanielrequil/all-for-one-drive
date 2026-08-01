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
	"all-for-one-drive/internal/oauth"
	"all-for-one-drive/internal/twofactor"
)

type Config struct {
	Providers []Provider `json:"providers"`
	UploadDir string     `json:"uploadDir"`
	Transfers int        `json:"transfers"` // rclone --transfers; 0 → 4
	BatchSize int        `json:"batchSize"` // файлов на один rclone copy; 0 → 32
}

type Provider struct {
	Name            string    `json:"name"`
	Type            string    `json:"type"`
	Priority        int       `json:"priority"`
	Echelon         int       `json:"echelon"`
	AcceptsArchives bool      `json:"acceptsArchives"`
	MaxUsageMB      int64     `json:"maxUsageMB"`    // 0 = без лимита; жёсткий потолок по rclone size
	MaxFileSizeMB   int64     `json:"maxFileSizeMB"` // 0 = без лимита; файлы больше — skip/spillover (Box free = 250)
	Accounts        []Account `json:"accounts"`
}

type Account struct {
	Name      string            `json:"name"`
	Email     string            `json:"email"` // только для человека; программа не использует
	RootPath  string            `json:"rootPath"`
	Options   map[string]string `json:"options"`
	OAuth     *OAuthConfig      `json:"oauth,omitempty"`
	TwoFactor *twofactor.Config `json:"twoFactor,omitempty"`
}

type OAuthConfig struct {
	TokenFile string `json:"tokenFile"`
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
	Echelon     int
	LoginOK     bool
	FreeBytes   *int64
	UsedBytes   *int64
	MaxUsageMB  int64
	Usage       string
	Error       string
}

type Client struct {
	binary     string
	configPath string
	transfers  int
	batchSize  int
	remotes    map[Target]remote
}

type remote struct {
	name           string
	backendType    string
	rootPath       string
	priority       int
	echelon        int
	archives       bool
	maxUsageBytes  int64
	maxFileBytes   int64
	availableBytes *int64
	email          string
	cloudName      string
	apiKey         string
	apiSecret      string
	tokenFile      string
	twoFactor      *twofactor.Config
}

type UploadItem struct {
	LocalPath    string
	RelativePath string
	Size         int64
}

type quota struct {
	Free  *int64 `json:"free"`
	Used  *int64 `json:"used"`
	Total *int64 `json:"total"`
}

type remoteSize struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

type cloudinaryUsage struct {
	Credits struct {
		Usage       float64 `json:"usage"`
		Limit       float64 `json:"limit"`
		UsedPercent float64 `json:"used_percent"`
	} `json:"credits"`
}

func New(ctx context.Context, config Config) (*Client, error) {
	applog.Entry("rclone", "New", "providers=%d", len(config.Providers))
	if len(config.Providers) == 0 {
		return nil, errors.New("providers must not be empty")
	}
	if strings.TrimSpace(config.UploadDir) == "" {
		return nil, errors.New("uploadDir is required")
	}
	transfers := config.Transfers
	if transfers == 0 {
		transfers = 4
	}
	if transfers < 1 {
		return nil, errors.New("transfers must be greater than zero")
	}
	batchSize := config.BatchSize
	if batchSize == 0 {
		batchSize = 32
	}
	if batchSize < 1 {
		return nil, errors.New("batchSize must be greater than zero")
	}

	binary, err := exec.LookPath("rclone")
	if err != nil {
		return nil, errors.New("rclone not found in PATH")
	}
	if err := prepareOAuthTokens(ctx, binary, config); err != nil {
		return nil, err
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
		"binary=%s configPath=%s transfers=%d batchSize=%d remotes=%d",
		binary,
		configPath,
		transfers,
		batchSize,
		len(remotes),
	)
	return &Client{
		binary:     binary,
		configPath: configPath,
		transfers:  transfers,
		batchSize:  batchSize,
		remotes:    remotes,
	}, nil
}

func prepareOAuthTokens(ctx context.Context, binary string, config Config) error {
	applog.Entry("rclone", "prepareOAuthTokens", "providers=%d", len(config.Providers))
	for providerIndex := range config.Providers {
		provider := &config.Providers[providerIndex]
		for accountIndex := range provider.Accounts {
			account := &provider.Accounts[accountIndex]
			if account.TwoFactor != nil {
				if _, err := twofactor.GenerateAndWrite(*account.TwoFactor); err != nil {
					return fmt.Errorf(
						"generate TOTP for provider %q account %q: %w",
						provider.Name,
						account.Name,
						err,
					)
				}
			}
			if account.OAuth == nil {
				continue
			}
			if strings.TrimSpace(account.OAuth.TokenFile) == "" {
				return fmt.Errorf(
					"provider %q account %q OAuth tokenFile is required",
					provider.Name,
					account.Name,
				)
			}
			if account.Options == nil {
				account.Options = make(map[string]string)
			}
			token, err := oauth.EnsureRcloneToken(
				ctx,
				binary,
				provider.Type,
				account.Options["client_id"],
				account.Options["client_secret"],
				account.OAuth.TokenFile,
			)
			if err != nil {
				return fmt.Errorf(
					"authorize provider %q account %q: %w",
					provider.Name,
					account.Name,
					err,
				)
			}
			account.Options["token"] = token
		}
	}
	return nil
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
			if strings.TrimSpace(account.Options["token"]) != "" {
				args = append(args, "config_refresh_token", "false")
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
	persistErr := c.persistOAuthTokens()
	removeErr := os.Remove(c.configPath)
	c.configPath = ""
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		removeErr = fmt.Errorf("remove temporary rclone config: %w", removeErr)
	} else {
		removeErr = nil
	}
	return errors.Join(persistErr, removeErr)
}

func (c *Client) persistOAuthTokens() error {
	content, err := os.ReadFile(c.configPath)
	if err != nil {
		return fmt.Errorf("read rclone config for OAuth token persistence: %w", err)
	}
	var result error
	for target, selected := range c.remotes {
		if selected.tokenFile == "" {
			continue
		}
		token, found := rcloneConfigOption(content, selected.name, "token")
		if !found {
			result = errors.Join(
				result,
				fmt.Errorf(
					"persist OAuth token for %q/%q: token is missing",
					target.Provider,
					target.Account,
				),
			)
			continue
		}
		if err := oauth.SaveToken(selected.tokenFile, token); err != nil {
			result = errors.Join(
				result,
				fmt.Errorf(
					"persist OAuth token for %q/%q: %w",
					target.Provider,
					target.Account,
					err,
				),
			)
		}
	}
	return result
}

func rcloneConfigOption(content []byte, section string, option string) (string, bool) {
	currentSection := ""
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			currentSection = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if currentSection != section {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if found && strings.EqualFold(strings.TrimSpace(key), option) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
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
		if left.echelon != right.echelon {
			return left.echelon < right.echelon
		}
		if left.priority != right.priority {
			return left.priority < right.priority
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
				Echelon:     c.remotes[target].echelon,
				Error:       ctx.Err().Error(),
			})
			continue
		}
		result := c.checkRemote(ctx, target)
		selected := c.remotes[target]
		selected.availableBytes = effectiveAvailableBytes(result, selected.maxUsageBytes)
		c.remotes[target] = selected
		results = append(results, result)
	}
	logCheckTable(results)
	return results
}

func effectiveAvailableBytes(result CheckResult, maxUsageBytes int64) *int64 {
	var available *int64
	if result.FreeBytes != nil {
		value := *result.FreeBytes
		available = &value
	}
	if maxUsageBytes > 0 && result.UsedBytes != nil {
		limitAvailable := maxUsageBytes - *result.UsedBytes
		if limitAvailable < 0 {
			limitAvailable = 0
		}
		if available == nil || limitAvailable < *available {
			available = &limitAvailable
		}
	}
	return available
}

// BestTarget returns the available account with the lowest numeric priority.
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
		if !result.LoginOK ||
			result.Priority == 0 ||
			result.Priority == 101 ||
			!strings.EqualFold(result.BackendType, backendType) {
			continue
		}
		if selected == nil || result.Priority < selected.Priority {
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

// TargetsForEchelon returns usable targets in ascending priority order.
// Priority 0 is administratively disabled; 101 is marked full. Both are still
// checked by PreCheck but never selected for upload.
func (c *Client) TargetsForEchelon(
	echelon int,
	results []CheckResult,
) []Target {
	applog.Entry(
		"rclone",
		"TargetsForEchelon",
		"echelon=%d results=%d",
		echelon,
		len(results),
	)
	filtered := make([]CheckResult, 0, len(results))
	for _, result := range results {
		if result.Echelon != echelon ||
			!result.LoginOK ||
			result.Priority == 0 ||
			result.Priority == 101 {
			continue
		}
		filtered = append(filtered, result)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Priority != filtered[j].Priority {
			return filtered[i].Priority < filtered[j].Priority
		}
		if filtered[i].Provider != filtered[j].Provider {
			return filtered[i].Provider < filtered[j].Provider
		}
		return filtered[i].Account < filtered[j].Account
	})
	targets := make([]Target, 0, len(filtered))
	for _, result := range filtered {
		targets = append(targets, Target{
			Provider: result.Provider,
			Account:  result.Account,
		})
	}
	return targets
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
	files, err := c.CollectUploadItems(sourcePaths)
	if err != nil {
		return err
	}
	_, err = c.UploadItems(ctx, target, files, destination)
	return err
}

// AvailableBytes returns the capacity measured during PreCheck. Nil means the
// backend did not expose a byte quota and no maxUsageMB is configured.
func (c *Client) AvailableBytes(target Target) (*int64, error) {
	selected, ok := c.remotes[target]
	if !ok {
		return nil, fmt.Errorf(
			"rclone target %q/%q is not configured",
			target.Provider,
			target.Account,
		)
	}
	if selected.availableBytes == nil {
		return nil, nil
	}
	value := *selected.availableBytes
	return &value, nil
}

// MaxFileSizeBytes returns the per-file upload limit. Nil means unlimited.
func (c *Client) MaxFileSizeBytes(target Target) (*int64, error) {
	selected, ok := c.remotes[target]
	if !ok {
		return nil, fmt.Errorf(
			"rclone target %q/%q is not configured",
			target.Provider,
			target.Account,
		)
	}
	if selected.maxFileBytes <= 0 {
		return nil, nil
	}
	value := selected.maxFileBytes
	return &value, nil
}

// UploadItems uploads an explicit subset while preserving each RelativePath.
// Files go to rclone in batches (batchSize); --transfers controls parallelism inside each batch.
func (c *Client) UploadItems(
	ctx context.Context,
	target Target,
	files []UploadItem,
	destination string,
) (int, error) {
	selected, ok := c.remotes[target]
	if !ok {
		return 0, fmt.Errorf("rclone target %q/%q is not configured", target.Provider, target.Account)
	}
	if err := c.ensureUploadWithinLimit(ctx, selected, files); err != nil {
		return 0, err
	}
	for _, file := range files {
		if selected.maxFileBytes > 0 && file.Size > selected.maxFileBytes {
			return 0, fmt.Errorf(
				"file %q (%s) exceeds maxFileSizeMB=%d",
				file.LocalPath,
				formatBytes(file.Size),
				selected.maxFileBytes/(1024*1024),
			)
		}
	}

	remoteRoot := joinRemotePath(selected.rootPath, destination)
	remoteDirectory := selected.name + ":" + remoteRoot
	if err := c.runStreamingForRemote(
		ctx,
		selected,
		"Upload.mkdir",
		"--config", c.configPath,
		"mkdir", remoteDirectory,
	); err != nil {
		return 0, fmt.Errorf("create remote directory %q: %w", remoteDirectory, err)
	}

	uploaded := 0
	var uploadedFiles []UploadItem
	for _, rootGroup := range groupUploadItemsByRoot(files) {
		for start := 0; start < len(rootGroup.files); start += c.batchSize {
			end := start + c.batchSize
			if end > len(rootGroup.files) {
				end = len(rootGroup.files)
			}
			batch := rootGroup.files[start:end]
			filesFromPath, err := writeFilesFromList(batch)
			if err != nil {
				c.consumeAvailableBytes(target, uploadedFiles)
				appendUploadMap(target.Provider, selected.email, uploadedFiles)
				return uploaded, err
			}
			applog.Entry(
				"rclone",
				"Upload.batch",
				"root=%s remote=%s files=%d transfers=%d",
				rootGroup.root,
				remoteDirectory,
				len(batch),
				c.transfers,
			)
			// transfers — параллелизм внутри пачки; metadata — mtime/хронология съёмки
			args := []string{
				"--config", c.configPath,
				"--transfers", strconv.Itoa(c.transfers),
				"--stats", "5s",
				"--stats-one-line",
				"copy",
				"--metadata",
				"--no-traverse",
				"--files-from", filesFromPath,
				rootGroup.root,
				remoteDirectory,
			}
			err = c.runStreamingForRemote(ctx, selected, "Upload", args...)
			_ = os.Remove(filesFromPath)
			if err != nil {
				c.consumeAvailableBytes(target, uploadedFiles)
				appendUploadMap(target.Provider, selected.email, uploadedFiles)
				return uploaded, fmt.Errorf(
					"upload batch (%d files) from %q to %q: %w",
					len(batch),
					rootGroup.root,
					remoteDirectory,
					err,
				)
			}
			uploaded += len(batch)
			uploadedFiles = append(uploadedFiles, batch...)
		}
	}
	c.consumeAvailableBytes(target, uploadedFiles)
	appendUploadMap(target.Provider, selected.email, uploadedFiles)
	return uploaded, nil
}

type uploadRootGroup struct {
	root  string
	files []UploadItem
}

func groupUploadItemsByRoot(files []UploadItem) []uploadRootGroup {
	indexByRoot := map[string]int{}
	var groups []uploadRootGroup
	for _, file := range files {
		root := localRootForUploadItem(file)
		if index, ok := indexByRoot[root]; ok {
			groups[index].files = append(groups[index].files, file)
			continue
		}
		indexByRoot[root] = len(groups)
		groups = append(groups, uploadRootGroup{
			root:  root,
			files: []UploadItem{file},
		})
	}
	return groups
}

func localRootForUploadItem(file UploadItem) string {
	root := filepath.Clean(file.LocalPath)
	rel := filepath.Clean(file.RelativePath)
	for rel != "." && rel != string(filepath.Separator) && rel != "" {
		root = filepath.Dir(root)
		rel = filepath.Dir(rel)
	}
	return root
}

func writeFilesFromList(files []UploadItem) (string, error) {
	tempFile, err := os.CreateTemp("", "all-for-one-files-from-*.txt")
	if err != nil {
		return "", fmt.Errorf("create files-from list: %w", err)
	}
	path := tempFile.Name()
	for _, file := range files {
		line := filepath.ToSlash(filepath.Clean(file.RelativePath))
		if _, err := fmt.Fprintln(tempFile, line); err != nil {
			tempFile.Close()
			os.Remove(path)
			return "", fmt.Errorf("write files-from list: %w", err)
		}
	}
	if err := tempFile.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close files-from list: %w", err)
	}
	return path, nil
}

// appendUploadMap writes folder~storage~email entries for a successful upload batch.
// Folder = top-level dir under image/video tree; if files sit at root — first file name.
func appendUploadMap(provider, email string, files []UploadItem) {
	if len(files) == 0 {
		return
	}
	keys := uploadMapFolderKeys(files)
	if len(keys) == 0 {
		return
	}
	if email == "" {
		email = "-"
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"~"+provider+"~"+email)
	}
	line := strings.Join(parts, " ")
	applog.UploadMap("%s", line)
	applog.Entry("rclone", "appendUploadMap", "%s", line)
}

func uploadMapFolderKeys(files []UploadItem) []string {
	seen := make(map[string]struct{})
	keys := make([]string, 0)
	rootFirst := ""
	for _, file := range files {
		relative := filepath.ToSlash(file.RelativePath)
		relative = strings.TrimPrefix(relative, "./")
		if relative == "" || relative == "." {
			continue
		}
		slash := strings.IndexByte(relative, '/')
		if slash < 0 {
			if rootFirst == "" {
				rootFirst = filepath.Base(relative)
			}
			continue
		}
		key := relative[:slash]
		if key == "" || key == "." {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if rootFirst != "" {
		if _, exists := seen[rootFirst]; !exists {
			keys = append(keys, rootFirst)
		}
	}
	return keys
}

func (c *Client) consumeAvailableBytes(target Target, files []UploadItem) {
	selected := c.remotes[target]
	if selected.availableBytes == nil {
		return
	}
	var uploadedBytes int64
	for _, file := range files {
		uploadedBytes += file.Size
	}
	remaining := *selected.availableBytes - uploadedBytes
	if remaining < 0 {
		remaining = 0
	}
	selected.availableBytes = &remaining
	c.remotes[target] = selected
}

func (c *Client) ensureUploadWithinLimit(
	ctx context.Context,
	selected remote,
	files []UploadItem,
) error {
	var uploadBytes int64
	for _, file := range files {
		uploadBytes += file.Size
	}
	if selected.availableBytes != nil && uploadBytes > *selected.availableBytes {
		return fmt.Errorf(
			"upload size %s exceeds available capacity %s",
			formatBytes(uploadBytes),
			formatBytes(*selected.availableBytes),
		)
	}
	if selected.maxUsageBytes <= 0 {
		return nil
	}
	usedBytes, err := c.remoteUsedBytes(ctx, selected)
	if err != nil {
		return fmt.Errorf("check usage limit: %w", err)
	}
	projected := usedBytes + uploadBytes
	applog.Entry(
		"rclone",
		"ensureUploadWithinLimit",
		"used=%d upload=%d projected=%d max=%d",
		usedBytes,
		uploadBytes,
		projected,
		selected.maxUsageBytes,
	)
	if projected > selected.maxUsageBytes {
		return fmt.Errorf(
			"upload would exceed maxUsageMB=%d (used=%s, upload=%s, limit=%s)",
			selected.maxUsageBytes/(1024*1024),
			formatBytes(usedBytes),
			formatBytes(uploadBytes),
			formatBytes(selected.maxUsageBytes),
		)
	}
	return nil
}

func (c *Client) remoteUsedBytes(ctx context.Context, selected remote) (int64, error) {
	remotePath := aboutRemotePath(selected)
	output, err := c.runCaptureForRemote(ctx, selected, "-vv", "size", "--json", remotePath)
	if err != nil {
		return 0, err
	}
	var size remoteSize
	if err := json.Unmarshal(output, &size); err != nil {
		return 0, fmt.Errorf("decode remote size: %w", err)
	}
	return size.Bytes, nil
}

func aboutRemotePath(selected remote) string {
	if selected.rootPath == "" {
		return selected.name + ":"
	}
	// For S3-compatible backends quota is reported on the bucket.
	bucket, _, _ := strings.Cut(selected.rootPath, "/")
	return selected.name + ":" + bucket
}

func (c *Client) CollectUploadItems(sourcePaths []string) ([]UploadItem, error) {
	applog.Entry("rclone", "CollectUploadItems", "sourcePaths=%v", sourcePaths)
	var files []UploadItem
	for _, sourcePath := range sourcePaths {
		info, err := os.Stat(sourcePath)
		if os.IsNotExist(err) {
			applog.Entry(
				"rclone",
				"CollectUploadItems",
				"skip missing source=%s",
				sourcePath,
			)
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect upload source %q: %w", sourcePath, err)
		}
		if !info.IsDir() {
			files = append(files, UploadItem{
				LocalPath:    sourcePath,
				RelativePath: filepath.Base(sourcePath),
				Size:         info.Size(),
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
			info, err := entry.Info()
			if err != nil {
				return err
			}
			files = append(files, UploadItem{
				LocalPath:    path,
				RelativePath: relativePath,
				Size:         info.Size(),
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("scan upload source %q: %w", sourcePath, err)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].LocalPath < files[j].LocalPath
	})
	applog.Entry("rclone", "CollectUploadItems", "files=%d", len(files))
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
		Echelon:     selected.echelon,
		MaxUsageMB:  selected.maxUsageBytes / (1024 * 1024),
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
		_, err = c.runCaptureForRemote(ctx, selected, "-vv", "mkdir", probePath)
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
	remotePath := aboutRemotePath(selected)
	applog.Entry("rclone", "checkRemote", "step=about start remote=%s", remotePath)
	output, err := c.runCaptureForRemote(ctx, selected, "-vv", "about", "--json", remotePath)
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
		result.UsedBytes = remoteQuota.Used
		if err := c.fillUsage(ctx, selected, &result); err != nil {
			result.LoginOK = false
			result.Error = compactError(err)
		}
		return result
	}
	applog.Entry("rclone", "checkRemote", "step=about failed err=%v fallback=lsd", err)

	// Some backends authenticate correctly but do not implement About.
	listPath := selected.name + ":"
	if selected.rootPath != "" {
		listPath = selected.name + ":" + selected.rootPath
	}
	applog.Entry("rclone", "checkRemote", "step=lsd start remote=%s maxDepth=1", listPath)
	_, listErr := c.runCaptureForRemote(
		ctx,
		selected,
		"-vv",
		"lsd",
		"--max-depth",
		"1",
		listPath,
	)
	if listErr == nil {
		applog.Entry("rclone", "checkRemote", "step=lsd success login=OK")
		result.LoginOK = true
		if err := c.fillUsage(ctx, selected, &result); err != nil {
			result.LoginOK = false
			result.Error = compactError(err)
		}
		return result
	}
	applog.Entry("rclone", "checkRemote", "step=lsd failed login=ERROR err=%v", listErr)
	result.Error = compactError(listErr)
	return result
}

// fillUsage measures occupancy with rclone size when About has no used bytes
// (S3/Storj/R2) and optionally enforces maxUsageMB.
func (c *Client) fillUsage(
	ctx context.Context,
	selected remote,
	result *CheckResult,
) error {
	needSize := selected.maxUsageBytes > 0 || result.UsedBytes == nil
	if !needSize {
		return nil
	}
	used, err := c.remoteUsedBytes(ctx, selected)
	if err != nil {
		if selected.maxUsageBytes > 0 {
			return fmt.Errorf("measure remote usage with rclone size: %w", err)
		}
		applog.Entry(
			"rclone",
			"fillUsage",
			"size unavailable err=%v",
			err,
		)
		return nil
	}
	result.UsedBytes = &used
	if selected.maxUsageBytes > 0 {
		result.Usage = fmt.Sprintf(
			"%s / %s (лимит)",
			formatBytes(used),
			formatBytes(selected.maxUsageBytes),
		)
		if used >= selected.maxUsageBytes {
			return fmt.Errorf(
				"maxUsageMB=%d exceeded (used=%s)",
				selected.maxUsageBytes/(1024*1024),
				formatBytes(used),
			)
		}
		return nil
	}
	result.Usage = fmt.Sprintf("%s занято", formatBytes(used))
	return nil
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

func (c *Client) runCaptureForRemote(
	ctx context.Context,
	selected remote,
	args ...string,
) ([]byte, error) {
	twoFactorArgs, err := rcloneTwoFactorArgs(selected)
	if err != nil {
		return nil, err
	}
	return c.runCapture(ctx, append(twoFactorArgs, args...)...)
}

func (c *Client) runStreamingForRemote(
	ctx context.Context,
	selected remote,
	function string,
	args ...string,
) error {
	twoFactorArgs, err := rcloneTwoFactorArgs(selected)
	if err != nil {
		return err
	}
	return c.runStreaming(ctx, function, append(twoFactorArgs, args...)...)
}

func rcloneTwoFactorArgs(selected remote) ([]string, error) {
	if selected.twoFactor == nil ||
		strings.TrimSpace(selected.twoFactor.RcloneOption) == "" {
		return nil, nil
	}
	code, err := twofactor.GenerateAndWrite(*selected.twoFactor)
	if err != nil {
		return nil, fmt.Errorf("generate rclone TOTP: %w", err)
	}
	option := strings.ReplaceAll(
		strings.TrimSpace(selected.twoFactor.RcloneOption),
		"_",
		"-",
	)
	flag := "--" + strings.ToLower(selected.backendType) + "-" + option
	return []string{flag, code}, nil
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
		if provider.Priority < 0 || provider.Priority > 101 {
			return nil, nil, fmt.Errorf("provider %q priority must be between 0 and 101", provider.Name)
		}
		if provider.Echelon < 1 || provider.Echelon > 3 {
			return nil, nil, fmt.Errorf("provider %q echelon must be between 1 and 3", provider.Name)
		}
		if provider.MaxUsageMB < 0 {
			return nil, nil, fmt.Errorf("provider %q maxUsageMB must be >= 0", provider.Name)
		}
		if provider.MaxFileSizeMB < 0 {
			return nil, nil, fmt.Errorf("provider %q maxFileSizeMB must be >= 0", provider.Name)
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
			tokenFile := ""
			if account.OAuth != nil {
				tokenFile = account.OAuth.TokenFile
			}
			remotes[target] = remote{
				name:          remoteName,
				backendType:   provider.Type,
				rootPath:      cleanRemotePath(account.RootPath),
				priority:      provider.Priority,
				echelon:       provider.Echelon,
				archives:      provider.AcceptsArchives,
				maxUsageBytes: provider.MaxUsageMB * 1024 * 1024,
				maxFileBytes:  provider.MaxFileSizeMB * 1024 * 1024,
				email:         strings.TrimSpace(account.Email),
				cloudName:     account.Options["cloud_name"],
				apiKey:        account.Options["api_key"],
				apiSecret:     account.Options["api_secret"],
				tokenFile:     tokenFile,
				twoFactor:     account.TwoFactor,
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
	stamp := time.Now().Format("2006-01-02 15:04:05")
	applog.Availability("")
	applog.Availability("=== PreCheck %s ===", stamp)
	header := fmt.Sprintf(
		"%-7s | %-9s | %-20s | %-20s | %-12s | %s",
		"ЭШЕЛОН",
		"ПРИОРИТЕТ",
		"ОБЛАКО",
		"АККАУНТ",
		"СТАТУС",
		"КВОТА / КРЕДИТЫ",
	)
	applog.Entry("rclone", "PreCheck", "%s", header)
	applog.Availability("%s", header)
	for _, result := range results {
		status := "OK"
		if !result.LoginOK {
			status = "ОШИБКА"
		}
		switch result.Priority {
		case 0:
			if result.LoginOK {
				status = "ОТКЛЮЧЕН"
			} else {
				status = "ОТКЛЮЧЕН+ОШИБКА"
			}
		case 101:
			if result.LoginOK {
				status = "ЗАПОЛНЕН"
			} else {
				status = "ЗАПОЛНЕН+ОШИБКА"
			}
		}
		free := "не поддерживается"
		if result.FreeBytes != nil {
			free = formatBytes(*result.FreeBytes)
		}
		if result.Usage != "" {
			free = result.Usage
		} else if result.UsedBytes != nil && result.MaxUsageMB > 0 {
			free = fmt.Sprintf(
				"%s / %s (лимит)",
				formatBytes(*result.UsedBytes),
				formatBytes(result.MaxUsageMB*1024*1024),
			)
		}
		if result.Error != "" {
			free = result.Error
		}
		line := fmt.Sprintf(
			"%-7d | %-9d | %-20s | %-20s | %-12s | %s",
			result.Echelon,
			result.Priority,
			result.Provider,
			result.Account,
			status,
			free,
		)
		applog.Entry("rclone", "PreCheck", "%s", line)
		applog.Availability("%s", line)
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
		lower := strings.ToLower(argument)
		if (strings.Contains(lower, "2fa") || strings.Contains(lower, "otp")) &&
			strings.HasPrefix(argument, "--") {
			if strings.Contains(argument, "=") {
				key, _, _ := strings.Cut(argument, "=")
				safe[index] = key + "=***"
			} else if index+1 < len(safe) {
				safe[index+1] = "***"
			}
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
