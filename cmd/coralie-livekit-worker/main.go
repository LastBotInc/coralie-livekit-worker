package main

import (
	"context"
	"os"

	"github.com/LastBotInc/coralie-livekit-worker/internal/config"
	"github.com/LastBotInc/coralie-livekit-worker/internal/logging"
	"github.com/LastBotInc/coralie-livekit-worker/internal/worker"
)

func main() {
	// Initialize logging
	logging.Init()
	defer logging.Shutdown(context.Background())

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		logging.Fail(logging.CategoryApp, "failed to load configuration: %v", err)
		os.Exit(1)
	}

	logging.Info(logging.CategoryApp, "starting coralie-livekit-worker version=%s", "dev")

	// Create worker
	w, err := worker.NewWorker(cfg)
	if err != nil {
		logging.Fail(logging.CategoryApp, "failed to create worker: %v", err)
		os.Exit(1)
	}

	// Start worker (blocks until shutdown)
	if err := w.Start(); err != nil {
		logging.Fail(logging.CategoryApp, "worker failed: %v", err)
		os.Exit(1)
	}

	logging.Info(logging.CategoryApp, "worker shutdown complete")
}
