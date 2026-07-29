package main

import (
	"context"
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
		fmt.Fprintf(os.Stderr, "convert-and-upload: %v\n", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	if err := applog.Init(); err != nil {
		return err
	}
	defer func() {
		applog.Error("convert-and-upload", "run", runErr)
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
		return fmt.Errorf("rclone cloudinary provider is not configured")
	}
	applog.Entry("convert-and-upload", "run", "stage=rclone pre-check")
	cloudClient, err := rcloneclient.New(globalConfig.Rclone)
	if err != nil {
		return fmt.Errorf("initialize rclone: %w", err)
	}
	defer cloudClient.Close()
	checkResults := cloudClient.PreCheck(ctx)
	cloudinaryTarget, err := cloudClient.BestTarget("cloudinary", checkResults)
	if err != nil {
		return fmt.Errorf("select Cloudinary account: %w", err)
	}

	applog.Entry("convert-and-upload", "run", "stage=image conversion")
	imageSummary, err := imageConverter.ProcessDir(ctx, printImageProgress)
	if err != nil {
		return err
	}
	printConversionSummary("image-converter", imageSummary.Total, imageSummary.Converted, imageSummary.Skipped, imageSummary.Failed)

	applog.Entry("convert-and-upload", "run", "stage=video conversion")
	videoSummary, err := videoConverter.ProcessDir(ctx, printVideoProgress)
	if err != nil {
		return err
	}
	printConversionSummary("video-converter", videoSummary.Total, videoSummary.Converted, videoSummary.Skipped, videoSummary.Failed)

	acceptsArchives, err := cloudClient.AcceptsArchives(cloudinaryTarget)
	if err != nil {
		return err
	}
	var uploadSources []string
	if acceptsArchives {
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
			map[string]string{
				"images": globalConfig.ImageConverter.OutputDir,
				"videos": globalConfig.VideoConverter.OutputDir,
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
		cloudinaryTarget.Provider,
		cloudinaryTarget.Account,
	)
	if err := cloudClient.Upload(
		ctx,
		cloudinaryTarget,
		uploadSources,
		"all-for-one",
	); err != nil {
		return fmt.Errorf("upload files to Cloudinary: %w", err)
	}
	if !acceptsArchives {
		if err := clearUploadDirectory(globalConfig.Rclone.UploadDir); err != nil {
			return fmt.Errorf("clear upload staging directory: %w", err)
		}
	}
	applog.Entry("convert-and-upload", "run", "done")
	return nil
}

func stageConvertedFiles(uploadDir string, sourceDirs map[string]string) error {
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
	for category, sourceDir := range sourceDirs {
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
			destination := filepath.Join(uploadDir, category, relativePath)
			if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
				return err
			}
			if _, err := os.Stat(destination); err == nil {
				return fmt.Errorf("upload staging file already exists: %s", destination)
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

func clearUploadDirectory(uploadDir string) error {
	applog.Entry(
		"convert-and-upload",
		"clearUploadDirectory",
		"uploadDir=%s",
		uploadDir,
	)
	if err := os.RemoveAll(uploadDir); err != nil {
		return err
	}
	return os.MkdirAll(uploadDir, 0o755)
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
