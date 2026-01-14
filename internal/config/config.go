package config

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
	"github.com/livekit/protocol/livekit"
)

// Config holds the configuration for the worker
type Config struct {
	// LiveKit configuration
	LiveKitURL          string
	LiveKitAPIKey       string
	LiveKitAPISecret    string
	AgentName           string
	Namespace           string
	JobType             livekit.JobType
	DrainTimeout        time.Duration
	MaxConcurrentJobs   int
	LogLevel            string
	PProfAddr           string
	LoadUpdateInterval  time.Duration
	JobTimeout          time.Duration

	// Coralie Conference Service configuration
	ConferenceGRPC      string
	ConferenceWorkerID  string
}

// Load loads configuration from environment variables and flags
func Load() (*Config, error) {
	cfg := &Config{}

	// Set defaults
	cfg.JobType = livekit.JobType_JT_ROOM
	cfg.DrainTimeout = 30 * time.Second
	cfg.MaxConcurrentJobs = 8
	cfg.LogLevel = "info"
	cfg.LoadUpdateInterval = 5 * time.Second
	cfg.JobTimeout = 5 * time.Minute
	cfg.ConferenceGRPC = ""
	cfg.ConferenceWorkerID = "coralie-livekit-worker-1"

	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("failed to load .env file: %w", err)
		}
	}

	// Load from environment
	cfg.LiveKitURL = getEnv("LIVEKIT_URL", "")
	cfg.LiveKitAPIKey = getEnv("LIVEKIT_API_KEY", "")
	cfg.LiveKitAPISecret = getEnv("LIVEKIT_API_SECRET", "")
	cfg.AgentName = getEnv("LK_AGENT_NAME", "")
	cfg.Namespace = getEnv("LK_NAMESPACE", "")
	cfg.PProfAddr = getEnv("LK_PPROF_ADDR", "")

	if jobTypeStr := getEnv("LK_JOB_TYPE", ""); jobTypeStr != "" {
		switch jobTypeStr {
		case "JT_ROOM":
			cfg.JobType = livekit.JobType_JT_ROOM
		case "JT_PUBLISHER":
			cfg.JobType = livekit.JobType_JT_PUBLISHER
		default:
			return nil, fmt.Errorf("invalid job type: %s (must be JT_ROOM or JT_PUBLISHER)", jobTypeStr)
		}
	}

	if drainTimeoutStr := getEnv("LK_DRAIN_TIMEOUT", ""); drainTimeoutStr != "" {
		if d, err := time.ParseDuration(drainTimeoutStr); err == nil {
			cfg.DrainTimeout = d
		}
	}

	if maxJobsStr := getEnv("LK_MAX_CONCURRENT_JOBS", ""); maxJobsStr != "" {
		if n, err := strconv.Atoi(maxJobsStr); err == nil && n > 0 {
			cfg.MaxConcurrentJobs = n
		}
	}

	if logLevel := getEnv("LK_LOG_LEVEL", ""); logLevel != "" {
		cfg.LogLevel = logLevel
	}

	if jobTimeoutStr := getEnv("LK_JOB_TIMEOUT", ""); jobTimeoutStr != "" {
		if d, err := time.ParseDuration(jobTimeoutStr); err == nil {
			cfg.JobTimeout = d
		}
	}

	cfg.ConferenceGRPC = getEnv("CONFERENCE_GRPC", cfg.ConferenceGRPC)
	cfg.ConferenceWorkerID = getEnv("CONFERENCE_WORKER_ID", cfg.ConferenceWorkerID)

	// Override with flags
	flag.StringVar(&cfg.LiveKitURL, "url", cfg.LiveKitURL, "LiveKit server URL")
	flag.StringVar(&cfg.LiveKitAPIKey, "api-key", cfg.LiveKitAPIKey, "LiveKit API key")
	flag.StringVar(&cfg.LiveKitAPISecret, "api-secret", cfg.LiveKitAPISecret, "LiveKit API secret")
	flag.StringVar(&cfg.AgentName, "agent-name", cfg.AgentName, "Agent name")
	flag.StringVar(&cfg.Namespace, "namespace", cfg.Namespace, "Namespace")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "Log level")
	flag.StringVar(&cfg.PProfAddr, "pprof-addr", cfg.PProfAddr, "pprof HTTP server address")
	flag.DurationVar(&cfg.DrainTimeout, "drain-timeout", cfg.DrainTimeout, "Drain timeout")
	flag.IntVar(&cfg.MaxConcurrentJobs, "max-jobs", cfg.MaxConcurrentJobs, "Maximum concurrent jobs")
	flag.DurationVar(&cfg.JobTimeout, "job-timeout", cfg.JobTimeout, "Maximum time a job can run")
	flag.StringVar(&cfg.ConferenceGRPC, "conference-grpc", cfg.ConferenceGRPC, "Coralie conference service gRPC address")
	flag.StringVar(&cfg.ConferenceWorkerID, "conference-worker-id", cfg.ConferenceWorkerID, "Worker ID for conference service")
	flag.Parse()

	// Validate required fields
	if cfg.LiveKitURL == "" {
		return nil, fmt.Errorf("LIVEKIT_URL is required")
	}
	if cfg.LiveKitAPIKey == "" {
		return nil, fmt.Errorf("LIVEKIT_API_KEY is required")
	}
	if cfg.LiveKitAPISecret == "" {
		return nil, fmt.Errorf("LIVEKIT_API_SECRET is required")
	}
	if cfg.ConferenceGRPC == "" {
		return nil, fmt.Errorf("CONFERENCE_GRPC is required")
	}

	return cfg, nil
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
