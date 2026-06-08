package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const pidFile = "/tmp/sccache-action-forwarder.pid"

func spawnForwarder() (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, err
	}
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "RUNNER_TRACKING_ID=") {
			continue // let it outlive the step
		}
		env = append(env, e)
	}
	env = append(env, "SCCACHE_ACTION_FORWARDER=1")
	cmd := exec.Command(exe)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func writePid(pid int) { _ = os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644) }

func readPid() int {
	b, err := os.ReadFile(pidFile)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func killPid(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func waitForEnvExport() bool {
	ge := os.Getenv("GITHUB_ENV")
	if ge == "" {
		return true
	}
	deadline := time.Now().Add(6 * time.Minute)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(ge); err == nil && strings.Contains(string(b), "SCCACHE_J=") {
			return true
		}
		time.Sleep(time.Second)
	}
	return false
}
