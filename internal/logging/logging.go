// Package logging provides a wrapper around coralie-logging-go/pkg/clog.
// All logging must go through clog - this package provides convenience wrappers.
package logging

import (
	"context"

	"github.com/LastBotInc/coralie-logging-go/pkg/clog"
)

// Category constants for consistent logging categories.
const (
	CategoryApp        = "App"
	CategoryWorker     = "Worker"
	CategoryJob        = "Job"
	CategoryBridge     = "Bridge"
	CategoryLiveKit    = "LiveKit"
	CategoryCoralie    = "Coralie"
	CategoryCodec      = "Codec"
	CategoryTranscribe = "Transcribe"
)

// Init initializes logging with default configuration.
func Init() {
	cfg := clog.DefaultConfig()
	cfg.Console.Enabled = true
	cfg.File.BaseDir = "./logs"
	cfg.Audio.Enabled = false
	clog.Init(cfg)
}

// Shutdown gracefully shuts down logging.
func Shutdown(ctx context.Context) {
	clog.Shutdown(ctx)
}

// Debug logs a debug message.
func Debug(category, msg string, params ...interface{}) {
	clog.Debug(category, msg, params...)
}

// Info logs an info message.
func Info(category, msg string, params ...interface{}) {
	clog.Info(category, msg, params...)
}

// Success logs a success message.
func Success(category, msg string, params ...interface{}) {
	clog.Success(category, msg, params...)
}

// Warning logs a warning message.
func Warning(category, msg string, params ...interface{}) {
	clog.Warning(category, msg, params...)
}

// Fail logs a failure message.
func Fail(category, msg string, params ...interface{}) {
	clog.Fail(category, msg, params...)
}

// Error logs an error message.
func Error(category, msg string, params ...interface{}) {
	clog.Error(category, msg, params...)
}

// Catastrophe logs a catastrophe message.
func Catastrophe(category, msg string, params ...interface{}) {
	clog.Catastrophe(category, msg, params...)
}
