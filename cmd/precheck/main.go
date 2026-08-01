package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"all-for-one-drive/internal/applog"
	"all-for-one-drive/internal/config"
	rcloneclient "all-for-one-drive/internal/rclone"
)

func main() {
	if err := run(); err != nil {
		applog.Error("precheck", "main", err)
		_ = applog.WriteErrorMarker(err)
		fmt.Fprintf(os.Stderr, "precheck: %v\n", err)
		os.Exit(1)
	}
}

func run() (runErr error) {
	if err := applog.Init(); err != nil {
		_ = applog.WriteErrorMarker(err)
		return err
	}
	defer func() {
		applog.Error("precheck", "run", runErr)
		if runErr != nil {
			_ = applog.WriteErrorMarker(runErr)
		}
		applog.Close()
	}()

	applog.Entry("precheck", "run", "start")
	defaultConfig, err := config.PathNextToExecutable()
	if err != nil {
		return fmt.Errorf("resolve default config path: %w", err)
	}
	configPath := flag.String("config", defaultConfig, "path to the global JSONC config")
	flag.Parse()
	applog.Entry("precheck", "run", "configPath=%s", *configPath)

	globalConfig, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if len(globalConfig.Rclone.Providers) == 0 {
		return fmt.Errorf("rclone provider is not configured")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cloudClient, err := rcloneclient.New(ctx, globalConfig.Rclone)
	if err != nil {
		return fmt.Errorf("initialize rclone: %w", err)
	}
	defer func() {
		if err := cloudClient.Close(); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("close rclone: %w", err))
		}
	}()

	results := cloudClient.PreCheck(ctx)
	failed := 0
	for _, result := range results {
		if !result.LoginOK {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("precheck failed for %d of %d account(s); see availability.log", failed, len(results))
	}
	applog.Entry("precheck", "run", "done accounts=%d", len(results))
	return nil
}
