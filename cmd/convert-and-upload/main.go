package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"

	"all-for-one-drive/internal/applog"
	"all-for-one-drive/internal/archiver"
	"all-for-one-drive/internal/config"
	imageconverter "all-for-one-drive/internal/image-converter"
	rcloneclient "all-for-one-drive/internal/rclone"
	videoconverter "all-for-one-drive/internal/video-converter"
)

type echelonPlan struct {
	echelon int
	targets []rcloneclient.Target
}

func main() {
	if err := run(); err != nil {
		applog.Error("convert-and-upload", "main", err)
		_ = applog.WriteErrorMarker(err)
		fmt.Fprintf(os.Stderr, "convert-and-upload: %v\n", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	if err := applog.Init(); err != nil {
		_ = applog.WriteErrorMarker(err)
		return err
	}
	defer func() {
		applog.Error("convert-and-upload", "run", runErr)
		if runErr != nil {
			_ = applog.WriteErrorMarker(runErr)
		}
		applog.Close()
	}()

	applog.Entry("convert-and-upload", "run", "start")
	defaultConfig, err := config.PathNextToExecutable()
	if err != nil {
		return fmt.Errorf("resolve default config path: %w", err)
	}
	configPath := flag.String("config", defaultConfig, "path to the global JSONC config")
	flag.Parse()
	applog.Entry("convert-and-upload", "run", "configPath=%s", *configPath)

	globalConfig, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	imageConverter, err := imageconverter.New(globalConfig.ImageConverter)
	if err != nil {
		return fmt.Errorf("invalid imageConverter config: %w", err)
	}
	videoConverter, err := videoconverter.New(globalConfig.VideoConverter)
	if err != nil {
		return fmt.Errorf("invalid videoConverter config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(globalConfig.Rclone.Providers) == 0 {
		return fmt.Errorf("rclone provider is not configured")
	}
	applog.Entry("convert-and-upload", "run", "stage=rclone pre-check")
	cloudClient, err := rcloneclient.New(ctx, globalConfig.Rclone)
	if err != nil {
		return fmt.Errorf("initialize rclone: %w", err)
	}
	defer func() {
		if err := cloudClient.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close rclone: %w", err))
		}
	}()
	checkResults := cloudClient.PreCheck(ctx)

	applog.Entry("convert-and-upload", "run", "stage=image conversion")
	imageSummary, err := imageConverter.ProcessDir(ctx, printImageProgress)
	if err != nil {
		return err
	}
	printConversionSummary("image-converter", imageSummary.Total, imageSummary.Converted, imageSummary.Skipped, imageSummary.Failed)
	if imageSummary.Failed > 0 {
		return fmt.Errorf("image conversion failed for %d file(s)", imageSummary.Failed)
	}

	applog.Entry("convert-and-upload", "run", "stage=video conversion")
	videoSummary, err := videoConverter.ProcessDir(ctx, printVideoProgress)
	if err != nil {
		return err
	}
	printConversionSummary("video-converter", videoSummary.Total, videoSummary.Converted, videoSummary.Skipped, videoSummary.Failed)
	if videoSummary.Failed > 0 {
		return fmt.Errorf("video conversion failed for %d file(s)", videoSummary.Failed)
	}

	convertedFiles, err := cloudClient.CollectUploadItems([]string{
		globalConfig.ImageConverter.OutputDir,
		globalConfig.VideoConverter.OutputDir,
	})
	if err != nil {
		return fmt.Errorf("scan converted outputs: %w", err)
	}
	if len(convertedFiles) == 0 {
		applog.Entry(
			"convert-and-upload",
			"run",
			"no files to upload; PreCheck completed and availability.log updated",
		)
		applog.Entry("convert-and-upload", "run", "done")
		return nil
	}

	plans, err := buildEchelonPlans(
		cloudClient,
		checkResults,
		globalConfig.Replication,
		globalConfig.Echelon,
	)
	if err != nil {
		return err
	}

	needArchives, needDirect, err := requiredRepresentations(
		cloudClient,
		plans,
		globalConfig.Archive,
	)
	if err != nil {
		return err
	}
	applog.Entry(
		"convert-and-upload",
		"run",
		"archive=%t needArchives=%t needDirect=%t replication=%d echelon=%d",
		globalConfig.Archive,
		needArchives,
		needDirect,
		globalConfig.Replication,
		globalConfig.Echelon,
	)
	directSources := []string{globalConfig.Rclone.UploadDir}
	archiveSources := []string{globalConfig.Archiver.OutputDir}
	if needDirect {
		applog.Entry(
			"convert-and-upload",
			"run",
			"stage=staging direct files uploadDir=%s",
			globalConfig.Rclone.UploadDir,
		)
		if err := stageConvertedFiles(
			globalConfig.Rclone.UploadDir,
			[]string{
				globalConfig.ImageConverter.OutputDir,
				globalConfig.VideoConverter.OutputDir,
			},
		); err != nil {
			return fmt.Errorf("stage converted files: %w", err)
		}
	}
	if needArchives {
		mediaArchiver, err := archiver.New(globalConfig.Archiver)
		if err != nil {
			return fmt.Errorf("invalid archiver config: %w", err)
		}
		applog.Entry("convert-and-upload", "run", "stage=archiving")
		archiveSummary, err := mediaArchiver.ArchiveDirectories(
			ctx,
			[]string{
				globalConfig.ImageConverter.OutputDir,
				globalConfig.VideoConverter.OutputDir,
			},
			"",
			printArchiveProgress,
		)
		if err != nil {
			return err
		}
		applog.Entry(
			"convert-and-upload",
			"run",
			"archiving done directories=%d files=%d archives=%d",
			archiveSummary.Directories,
			archiveSummary.Files,
			archiveSummary.Archives,
		)
	}

	if err := uploadReplicas(
		ctx,
		cloudClient,
		plans,
		globalConfig.Archive,
		archiveSources,
		directSources,
	); err != nil {
		return err
	}
	if needArchives {
		if err := clearDirectory(globalConfig.Archiver.OutputDir); err != nil {
			return fmt.Errorf("clear archive output directory: %w", err)
		}
	}
	if needDirect {
		if err := clearDirectory(globalConfig.Rclone.UploadDir); err != nil {
			return fmt.Errorf("clear upload staging directory: %w", err)
		}
	}
	if err := clearDirectory(globalConfig.ImageConverter.OutputDir); err != nil {
		return fmt.Errorf("clear image output directory: %w", err)
	}
	if err := clearDirectory(globalConfig.VideoConverter.OutputDir); err != nil {
		return fmt.Errorf("clear video output directory: %w", err)
	}
	applog.Entry("convert-and-upload", "run", "done")
	return nil
}

func buildEchelonPlans(
	client *rcloneclient.Client,
	results []rcloneclient.CheckResult,
	replication int,
	startEchelon int,
) ([]echelonPlan, error) {
	lastEchelon := startEchelon + replication - 1
	if lastEchelon > 3 {
		lastEchelon = 3
	}
	plans := make([]echelonPlan, 0, lastEchelon-startEchelon+1)
	var missing []int
	for echelon := startEchelon; echelon <= lastEchelon; echelon++ {
		targets := client.TargetsForEchelon(echelon, results)
		if len(targets) == 0 {
			missing = append(missing, echelon)
			applog.Entry(
				"convert-and-upload",
				"buildEchelonPlans",
				"echelon=%d skipped: no available storage with priority 1..100",
				echelon,
			)
			continue
		}
		plans = append(plans, echelonPlan{
			echelon: echelon,
			targets: targets,
		})
	}
	if len(plans) == 0 {
		return nil, fmt.Errorf(
			"no available storage in echelons %d..%d",
			startEchelon,
			lastEchelon,
		)
	}
	if len(missing) > 0 {
		applog.Entry(
			"convert-and-upload",
			"buildEchelonPlans",
			"replication incomplete: missing echelons=%v available=%d",
			missing,
			len(plans),
		)
	}
	return plans, nil
}

func requiredRepresentations(
	client *rcloneclient.Client,
	plans []echelonPlan,
	archiveEnabled bool,
) (needArchives bool, needDirect bool, err error) {
	if !archiveEnabled {
		return false, true, nil
	}
	for _, plan := range plans {
		for _, target := range plan.targets {
			accepts, targetErr := client.AcceptsArchives(target)
			if targetErr != nil {
				return false, false, targetErr
			}
			if accepts {
				needArchives = true
			} else {
				needDirect = true
			}
		}
	}
	return needArchives, needDirect, nil
}

func uploadReplicas(
	ctx context.Context,
	client *rcloneclient.Client,
	plans []echelonPlan,
	archiveEnabled bool,
	archiveSources []string,
	directSources []string,
) error {
	for _, plan := range plans {
		useArchives, err := echelonUsesArchives(client, plan.targets, archiveEnabled)
		if err != nil {
			return err
		}
		sources := directSources
		representation := "direct"
		if useArchives {
			sources = archiveSources
			representation = "archives"
		}
		remaining, err := client.CollectUploadItems(sources)
		if err != nil {
			return fmt.Errorf("collect %s for echelon %d: %w", representation, plan.echelon, err)
		}
		if len(remaining) == 0 {
			applog.Entry(
				"convert-and-upload",
				"uploadReplicas",
				"echelon=%d representation=%s nothing to upload",
				plan.echelon,
				representation,
			)
			continue
		}
		var attemptErrors error
		for _, target := range plan.targets {
			acceptsArchives, err := client.AcceptsArchives(target)
			if err != nil {
				attemptErrors = errors.Join(attemptErrors, err)
				continue
			}
			// Архивы — только на acceptsArchives; прямые файлы принимают все.
			if useArchives && !acceptsArchives {
				applog.Entry(
					"convert-and-upload",
					"uploadReplicas",
					"echelon=%d provider=%s skipped: archives required but acceptsArchives=false",
					plan.echelon,
					target.Provider,
				)
				continue
			}
			available, err := client.AvailableBytes(target)
			if err != nil {
				attemptErrors = errors.Join(attemptErrors, err)
				continue
			}
			maxFileSize, err := client.MaxFileSizeBytes(target)
			if err != nil {
				attemptErrors = errors.Join(attemptErrors, err)
				continue
			}
			selected, deferred := fitUploadItems(remaining, available, maxFileSize)
			if len(selected) == 0 && len(remaining) > 0 {
				applog.Entry(
					"convert-and-upload",
					"uploadReplicas",
					"echelon=%d provider=%s account=%s no files fit (quota or maxFileSize)",
					plan.echelon,
					target.Provider,
					target.Account,
				)
				remaining = deferred
				if len(deferred) == 0 {
					attemptErrors = errors.Join(
						attemptErrors,
						fmt.Errorf("%s/%s: no remaining file fits", target.Provider, target.Account),
					)
				}
				continue
			}
			applog.Entry(
				"convert-and-upload",
				"uploadReplicas",
				"echelon=%d provider=%s account=%s representation=%s files=%d remaining=%d",
				plan.echelon,
				target.Provider,
				target.Account,
				representation,
				len(selected),
				len(deferred),
			)
			uploadedCount, err := client.UploadItems(
				ctx,
				target,
				selected,
				"all-for-one",
			)
			if err != nil {
				remaining = append(
					append(
						[]rcloneclient.UploadItem(nil),
						selected[uploadedCount:]...,
					),
					deferred...,
				)
				applog.Entry(
					"convert-and-upload",
					"uploadReplicas",
					"echelon=%d provider=%s account=%s uploaded=%d failed=%v",
					plan.echelon,
					target.Provider,
					target.Account,
					uploadedCount,
					err,
				)
				attemptErrors = errors.Join(
					attemptErrors,
					fmt.Errorf("%s/%s: %w", target.Provider, target.Account, err),
				)
				continue
			}
			remaining = deferred
			if len(remaining) == 0 {
				break
			}
		}
		if len(remaining) > 0 {
			return fmt.Errorf(
				"upload replica to echelon %d incomplete (%d files remain): %w",
				plan.echelon,
				len(remaining),
				attemptErrors,
			)
		}
	}
	return nil
}

func echelonUsesArchives(
	client *rcloneclient.Client,
	targets []rcloneclient.Target,
	archiveEnabled bool,
) (bool, error) {
	if !archiveEnabled {
		return false, nil
	}
	for _, target := range targets {
		accepts, err := client.AcceptsArchives(target)
		if err != nil {
			return false, err
		}
		if accepts {
			return true, nil
		}
	}
	return false, nil
}

func fitUploadItems(
	items []rcloneclient.UploadItem,
	available *int64,
	maxFileSize *int64,
) (selected []rcloneclient.UploadItem, deferred []rcloneclient.UploadItem) {
	var remainingBytes int64
	limitBytes := available != nil
	if limitBytes {
		remainingBytes = *available
	}
	for _, item := range items {
		if maxFileSize != nil && item.Size > *maxFileSize {
			deferred = append(deferred, item)
			continue
		}
		if limitBytes {
			if item.Size <= remainingBytes {
				selected = append(selected, item)
				remainingBytes -= item.Size
			} else {
				deferred = append(deferred, item)
			}
			continue
		}
		selected = append(selected, item)
	}
	return selected, deferred
}

func stageConvertedFiles(uploadDir string, sourceDirs []string) error {
	applog.Entry(
		"convert-and-upload",
		"stageConvertedFiles",
		"uploadDir=%s sourceDirs=%v",
		uploadDir,
		sourceDirs,
	)
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		return err
	}
	for _, sourceDir := range sourceDirs {
		_, err := os.Stat(sourceDir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			relativePath, err := filepath.Rel(sourceDir, path)
			if err != nil {
				return err
			}
			destination := filepath.Join(uploadDir, relativePath)
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			if _, err := os.Stat(destination); err == nil {
				applog.Entry(
					"convert-and-upload",
					"stageConvertedFiles",
					"skip existing destination=%s",
					destination,
				)
				return nil
			} else if !os.IsNotExist(err) {
				return err
			}
			applog.Entry(
				"convert-and-upload",
				"stageConvertedFiles",
				"copy source=%s destination=%s",
				path,
				destination,
			)
			return copyFile(path, destination)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(
		destination,
		os.O_CREATE|os.O_EXCL|os.O_WRONLY,
		info.Mode().Perm(),
	)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(destination)
		return errors.Join(copyErr, closeErr)
	}
	modTime := info.ModTime()
	if err := os.Chtimes(destination, modTime, modTime); err != nil {
		_ = os.Remove(destination)
		return err
	}
	return nil
}

func clearDirectory(path string) error {
	applog.Entry(
		"convert-and-upload",
		"clearDirectory",
		"path=%s",
		path,
	)
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	return os.MkdirAll(path, 0o755)
}

func printImageProgress(progress imageconverter.Progress) {
	printConversionProgress(
		"image-converter",
		progress.Current,
		progress.Total,
		progress.SourcePath,
		progress.OutputPath,
		progress.Skipped,
		progress.Err,
	)
}

func printVideoProgress(progress videoconverter.Progress) {
	printConversionProgress(
		"video-converter",
		progress.Current,
		progress.Total,
		progress.SourcePath,
		progress.OutputPath,
		progress.Skipped,
		progress.Err,
	)
}

func printConversionProgress(
	module string,
	current int,
	total int,
	sourcePath string,
	outputPath string,
	skipped bool,
	err error,
) {
	applog.Entry(
		module,
		"progress",
		"current=%d total=%d source=%s output=%s skipped=%t err=%v",
		current,
		total,
		sourcePath,
		outputPath,
		skipped,
		err,
	)
}

func printConversionSummary(module string, total, converted, skipped, failed int) {
	applog.Entry(
		module,
		"summary",
		"total=%d converted=%d skipped=%d failed=%d",
		total,
		converted,
		skipped,
		failed,
	)
}

func printArchiveProgress(progress archiver.Progress) {
	applog.Entry(
		"archiver",
		"progress",
		"sourceDir=%s archive=%s files=%d size=%d",
		progress.SourceDir,
		progress.ArchivePath,
		progress.FileCount,
		progress.Size,
	)
}
