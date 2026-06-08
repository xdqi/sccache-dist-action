package coordinator

import "testing"

func TestWorkerPort(t *testing.T) {
	cases := map[string]int{
		"123-worker-1": 10501,
		"123-worker-2": 10502,
		"123-worker-3": 10503,
		"123-worker-x": 10501,
	}
	for host, want := range cases {
		if got := workerPort(host); got != want {
			t.Fatalf("%s: got %d want %d", host, got, want)
		}
	}
}

func TestBoolStr(t *testing.T) {
	if boolStr(true) != "1" || boolStr(false) != "0" {
		t.Fatal("boolStr")
	}
}
