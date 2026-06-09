package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Mode            string
	OAuthSecret     string
	Tags            string
	RunPrefix       string
	ExpectedWorkers int
	MinWorkers      int
	WaitTimeout     time.Duration
	Slots           int
	PollInterval    time.Duration
	TeardownThresh  int
	DistFallback    bool
	// ServerLog is the env_logger directive injected as SCCACHE_LOG into the
	// scheduler and server processes. sccache-dist only initializes its logger
	// when SCCACHE_LOG is set, so this is what makes per-worker build logs
	// visible. Empty disables logging entirely. Default "debug" surfaces the
	// build lifecycle (toolchain load, performing build, docker ops); use
	// "trace" to also see each compile command.
	ServerLog string
}

func Load() (*Config, error) { return loadFrom(os.Getenv) }

func loadFrom(get func(string) string) (*Config, error) {
	c := &Config{
		Mode:           get("INPUT_MODE"),
		OAuthSecret:    get("INPUT_OAUTH_SECRET"),
		Tags:           orDefault(get("INPUT_TAGS"), "tag:ci-sccache"),
		RunPrefix:      orDefault(get("INPUT_RUN_PREFIX"), get("GITHUB_RUN_ID")),
		Slots:          atoiOr(get("INPUT_SLOTS"), 0),
		PollInterval:   durOr(get("INPUT_POLL_INTERVAL"), time.Second),
		TeardownThresh: atoiOr(get("INPUT_TEARDOWN_THRESHOLD"), 5),
		WaitTimeout:    durOr(get("INPUT_WAIT_TIMEOUT"), 300*time.Second),
		DistFallback:   boolOr(get("INPUT_DIST_FALLBACK"), true),
		ServerLog:      orDefault(get("INPUT_SERVER_LOG"), "debug"),
	}
	if c.Mode != "coordinator" && c.Mode != "worker" {
		return nil, fmt.Errorf("mode must be coordinator|worker, got %q", c.Mode)
	}
	if c.OAuthSecret == "" {
		return nil, fmt.Errorf("oauth-secret is required")
	}
	if c.RunPrefix == "" {
		return nil, fmt.Errorf("run-prefix empty and GITHUB_RUN_ID unset")
	}
	if c.Mode == "coordinator" {
		c.ExpectedWorkers = atoiOr(get("INPUT_EXPECTED_WORKERS"), 0)
		if c.ExpectedWorkers < 1 {
			return nil, fmt.Errorf("expected-workers must be >= 1 for coordinator")
		}
		c.MinWorkers = atoiOr(get("INPUT_MIN_WORKERS"), c.ExpectedWorkers)
		if c.MinWorkers < 1 || c.MinWorkers > c.ExpectedWorkers {
			return nil, fmt.Errorf("min-workers must be in [1, expected-workers]")
		}
	}
	return c, nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
func atoiOr(v string, d int) int {
	if v == "" {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n
	}
	return d
}
func boolOr(v string, d bool) bool {
	if v == "" {
		return d
	}
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return d
}
func durOr(v string, d time.Duration) time.Duration {
	if v == "" {
		return d
	}
	if dur, err := time.ParseDuration(v); err == nil {
		return dur
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	return d
}
