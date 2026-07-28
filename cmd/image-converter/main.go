package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"all-for-one-drive/internal/config"
	imageconverter "all-for-one-drive/internal/image-converter"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "image-converter: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	defaultConfig, err := configPathNextToExecutable()
	if err != nil {
		return fmt.Errorf("resolve default config path: %w", err)
	}
	configPath := flag.String("config", defaultConfig, "path to the global JSONC config")
	flag.Parse()

	globalConfig, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	converter, err := imageconverter.New(globalConfig.ImageConverter)
	if err != nil {
		return fmt.Errorf("invalid imageConverter config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	summary, err := converter.ProcessDir(ctx, printProgress)
	if err != nil {
		return err
	}
	fmt.Printf(
		"Готово: всего %d, обработано %d, пропущено %d, ошибок %d\n",
		summary.Total,
		summary.Converted,
		summary.Skipped,
		summary.Failed,
	)
	return nil
}

func configPathNextToExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(exe), "all-for-one.config.jsonc"), nil
}

func printProgress(progress imageconverter.Progress) {
	prefix := fmt.Sprintf("[%d/%d]", progress.Current, progress.Total)
	switch {
	case progress.Err != nil:
		fmt.Fprintf(os.Stderr, "%s ОШИБКА %s: %v (файл пропущен)\n", prefix, progress.SourcePath, progress.Err)
	case progress.Skipped:
		fmt.Printf("%s ПРОПУСК %s (результат уже существует)\n", prefix, progress.SourcePath)
	default:
		fmt.Printf("%s OK %s -> %s\n", prefix, progress.SourcePath, progress.OutputPath)
	}
}
