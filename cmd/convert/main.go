package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"all-for-one-drive/internal/applog"
	"all-for-one-drive/internal/config"
	imageconverter "all-for-one-drive/internal/image-converter"
	videoconverter "all-for-one-drive/internal/video-converter"
)

func main() {
	if err := run(); err != nil {
		applog.Error("convert", "main", err)
		_ = applog.WriteErrorMarker(err)
		fmt.Fprintf(os.Stderr, "convert: %v\n", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	if err := applog.Init(); err != nil {
		_ = applog.WriteErrorMarker(err)
		return err
	}
	defer func() {
		applog.Error("convert", "run", runErr)
		if runErr != nil {
			_ = applog.WriteErrorMarker(runErr)
		}
		applog.Close()
	}()

	applog.Entry("convert", "run", "start")
	defaultConfig, err := config.PathNextToExecutable()
	if err != nil {
		return fmt.Errorf("resolve default config path: %w", err)
	}
	configPath := flag.String("config", defaultConfig, "path to the global JSONC config")
	flag.Parse()
	applog.Entry("convert", "run", "configPath=%s", *configPath)

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

	applog.Entry("convert", "run", "stage=image conversion")
	imageSummary, err := imageConverter.ProcessDir(ctx, printImageProgress)
	if err != nil {
		return err
	}
	printConversionSummary(
		"image-converter",
		imageSummary.Total,
		imageSummary.Converted,
		imageSummary.Skipped,
		imageSummary.Failed,
	)
	if imageSummary.Failed > 0 {
		return fmt.Errorf("image conversion failed for %d file(s)", imageSummary.Failed)
	}

	applog.Entry("convert", "run", "stage=video conversion")
	videoSummary, err := videoConverter.ProcessDir(ctx, printVideoProgress)
	if err != nil {
		return err
	}
	printConversionSummary(
		"video-converter",
		videoSummary.Total,
		videoSummary.Converted,
		videoSummary.Skipped,
		videoSummary.Failed,
	)
	if videoSummary.Failed > 0 {
		return fmt.Errorf("video conversion failed for %d file(s)", videoSummary.Failed)
	}

	applog.Entry(
		"convert",
		"run",
		"done images=%d videos=%d",
		imageSummary.Converted+imageSummary.Skipped,
		videoSummary.Converted+videoSummary.Skipped,
	)
	return nil
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
