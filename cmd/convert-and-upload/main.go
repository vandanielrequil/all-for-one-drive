package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"all-for-one-drive/internal/applog"
	"all-for-one-drive/internal/archiver"
	"all-for-one-drive/internal/config"
	imageconverter "all-for-one-drive/internal/image-converter"
	videoconverter "all-for-one-drive/internal/video-converter"
)

func main() {
	if err := run(); err != nil {
		applog.Error("convert-and-upload", "main", err)
		fmt.Fprintf(os.Stderr, "convert-and-upload: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := applog.Init(); err != nil {
		return err
	}
	defer applog.Close()

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
	mediaArchiver, err := archiver.New(globalConfig.Archiver)
	if err != nil {
		return fmt.Errorf("invalid archiver config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

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
		"done directories=%d files=%d archives=%d",
		archiveSummary.Directories,
		archiveSummary.Files,
		archiveSummary.Archives,
	)
	return nil
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
