package config

import "testing"

func TestLoadCoordinator(t *testing.T) {
	env := map[string]string{
		"INPUT_MODE": "coordinator", "INPUT_OAUTH_SECRET": "k",
		"INPUT_RUN_PREFIX": "r1", "INPUT_EXPECTED_WORKERS": "3",
	}
	c, err := loadFrom(func(k string) string { return env[k] })
	if err != nil { t.Fatal(err) }
	if c.ExpectedWorkers != 3 || c.MinWorkers != 3 || !c.DistFallback {
		t.Fatalf("got %+v", c)
	}
	if c.ServerLog != "debug" {
		t.Fatalf("ServerLog default = %q, want debug", c.ServerLog)
	}
}

func TestServerLogOverride(t *testing.T) {
	env := map[string]string{
		"INPUT_MODE": "worker", "INPUT_OAUTH_SECRET": "k",
		"INPUT_RUN_PREFIX": "r1", "INPUT_SERVER_LOG": "trace",
	}
	c, err := loadFrom(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.ServerLog != "trace" {
		t.Fatalf("ServerLog = %q, want trace", c.ServerLog)
	}
}

func TestLoadRejectsBadMode(t *testing.T) {
	_, err := loadFrom(func(k string) string { if k == "INPUT_MODE" { return "x" }; return "" })
	if err == nil { t.Fatal("expected error") }
}
