package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usage"
)

// runUsageCommand runs -usage-push or -usage-import with the config file's usage section.
func runUsageCommand(configPath, pushURL, importDir string) int {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: %v\n", err)
		return 1
	}
	cfg, err := config.ParseConfigBytes(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "usage: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if importDir != "" {
		results, err := usage.ImportTokenTracker(ctx, cfg.Usage, importDir)
		for device, result := range results {
			fmt.Printf("%s: %d records imported\n", device, result.Accepted)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "usage import: %v\n", err)
			return 1
		}
		return 0
	}
	if err = usage.Push(ctx, cfg.Usage, pushURL); err != nil {
		fmt.Fprintf(os.Stderr, "usage push: %v\n", err)
		return 1
	}
	return 0
}
