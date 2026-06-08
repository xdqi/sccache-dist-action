package sccachedist

import (
	"strings"
	"testing"
)

func TestSchedulerConf(t *testing.T) {
	c := SchedulerConf("tok")
	if !strings.Contains(c, `token = "tok"`) || !strings.Contains(c, "0.0.0.0:10600") {
		t.Fatalf("bad conf:\n%s", c)
	}
}

func TestServerConf(t *testing.T) {
	c := ServerConf("tok", "127.0.0.1:10501", "http://127.0.0.1:10600", "/c")
	for _, want := range []string{`public_addr = "127.0.0.1:10501"`, `type = "docker"`, `scheduler_url = "http://127.0.0.1:10600"`} {
		if !strings.Contains(c, want) {
			t.Fatalf("missing %q in:\n%s", want, c)
		}
	}
}

func TestClientConfig(t *testing.T) {
	c := ClientConfig("tok", "http://127.0.0.1:10600")
	if !strings.Contains(c, `[dist]`) || !strings.Contains(c, `token = "tok"`) {
		t.Fatalf("bad client config:\n%s", c)
	}
}

func TestTotalJ(t *testing.T) {
	if TotalJ(3, 4) != 12 {
		t.Fatal("3*4=12")
	}
}

func TestWriteFile(t *testing.T) {
	p := t.TempDir() + "/sub/conf.toml"
	if err := WriteFile(p, "x=1"); err != nil {
		t.Fatal(err)
	}
}
