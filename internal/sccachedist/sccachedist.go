package sccachedist

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ConfPath returns a writable path for a generated config file. GitHub runners
// run the action as a non-root user with a read-only /run, so use RUNNER_TEMP
// (or the OS temp dir) instead of /run.
func ConfPath(name string) string {
	dir := os.Getenv("RUNNER_TEMP")
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, name)
}

// Nproc is the default per-worker slot count.
func Nproc() int { return runtime.NumCPU() }

// TotalJ is the suggested -j for the coordinator build (sum of worker slots).
func TotalJ(numWorkers, slots int) int { return numWorkers * slots }

// SchedulerConf renders the scheduler config with the shared token.
func SchedulerConf(token string) string {
	return fmt.Sprintf(`public_addr = "0.0.0.0:10600"
[client_auth]
type = "token"
token = %q
[server_auth]
type = "token"
token = %q
`, token, token)
}

// ServerConf renders the server config. publicAddr is the address the CLIENT
// uses to reach this server (per M0 RESULT: coordinator-local 127.0.0.1:<port>);
// schedulerURL is reachable from the worker over tsnet.
func ServerConf(token, publicAddr, schedulerURL, cacheDir string) string {
	return fmt.Sprintf(`cache_dir = %q
public_addr = %q
bind_address = "0.0.0.0:10501"
scheduler_url = %q
[builder]
type = "docker"
[scheduler_auth]
type = "token"
token = %q
`, cacheDir, publicAddr, schedulerURL, token)
}

// ClientConfig renders ~/.config/sccache/config for the coordinator's client.
func ClientConfig(token, schedulerURL string) string {
	return fmt.Sprintf(`[dist]
scheduler_url = %q
toolchain_cache_size = 5368709120
[dist.auth]
type = "token"
token = %q
`, schedulerURL, token)
}

// StartScheduler launches `sccache-dist scheduler` in the foreground (caller
// keeps the *exec.Cmd to kill on teardown). Logs to stderr/stdout.
func StartScheduler(confPath string) (*exec.Cmd, error) {
	cmd := exec.Command("sccache-dist", "scheduler", "--config", confPath)
	cmd.Env = append(os.Environ(), "SCCACHE_NO_DAEMON=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start scheduler: %w", err)
	}
	return cmd, nil
}

// StartServer launches `sccache-dist server`.
func StartServer(confPath string) (*exec.Cmd, error) {
	cmd := exec.Command("sccache-dist", "server", "--config", confPath)
	cmd.Env = append(os.Environ(), "SCCACHE_NO_DAEMON=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	return cmd, nil
}

// WriteFile writes content to path (0644), creating parent dirs.
func WriteFile(path, content string) error {
	if err := os.MkdirAll(dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

func dir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
