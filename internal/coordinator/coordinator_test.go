package coordinator

import "testing"

func TestForwardSpec(t *testing.T) {
	cases := map[int]struct {
		port   int
		target string
	}{
		1: {10501, "123-1-worker-1:10501"},
		2: {10502, "123-1-worker-2:10501"},
		3: {10503, "123-1-worker-3:10501"},
	}
	for idx, want := range cases {
		port, target := forwardSpec("123-1-worker-", idx)
		if port != want.port || target != want.target {
			t.Fatalf("idx %d: got (%d, %s) want (%d, %s)", idx, port, target, want.port, want.target)
		}
	}
}

func TestBoolStr(t *testing.T) {
	if boolStr(true) != "1" || boolStr(false) != "0" {
		t.Fatal("boolStr")
	}
}
