package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
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
	defaultConfig, err := configPathNextToExecutable()
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
	uploadTarget, err := cloudClient.BestTarget("drive", checkResults)
	if err != nil {
		return fmt.Errorf("select Google Drive account: %w", err)
	}

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

	acceptsArchives, err := cloudClient.AcceptsArchives(uploadTarget)
	if err != nil {
		return err
	}
	shouldArchive := globalConfig.Archive && acceptsArchives
	applog.Entry(
		"convert-and-upload",
		"run",
		"archive=%t providerAcceptsArchives=%t shouldArchive=%t",
		globalConfig.Archive,
		acceptsArchives,
		shouldArchive,
	)
	var uploadSources []string
	if shouldArchive {
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
		uploadSources = []string{globalConfig.Archiver.OutputDir}
	} else {
		applog.Entry(
			"convert-and-upload",
			"run",
			"stage=staging without archives uploadDir=%s",
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
		uploadSources = []string{globalConfig.Rclone.UploadDir}
	}

	applog.Entry(
		"convert-and-upload",
		"run",
		"stage=upload provider=%s account=%s destination=all-for-one",
		uploadTarget.Provider,
		uploadTarget.Account,
	)
	if err := cloudClient.Upload(
		ctx,
		uploadTarget,
		uploadSources,
		"all-for-one",
	); err != nil {
		return fmt.Errorf("upload files: %w", err)
	}
	if shouldArchive {
		if err := clearDirectory(globalConfig.Archiver.OutputDir); err != nil {
			return fmt.Errorf("clear archive output directory: %w", err)
		}
	} else {
		if err := clearDirectory(globalConfig.Rclone.UploadDir); err != nil {
			return fmt.Errorf("clear upload staging directory: %w", err)
		}
	}
	applog.Entry("convert-and-upload", "run", "done")
	return nil
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
				"move source=%s destination=%s",
				path,
				destination,
			)
			return os.Rename(path, destination)
		})
		if err != nil {
			return err
		}
		if err := os.RemoveAll(sourceDir); err != nil {
			return err
		}
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

func configPathNextToExecutable() (string, error) {
	applog.Entry("convert-and-upload", "configPathNextToExecutable", "start")
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	path := filepath.Join(filepath.Dir(executable), "all-for-one.config.jsonc")
	applog.Entry("convert-and-upload", "configPathNextToExecutable", "path=%s", path)
	return path, nil
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
