package client

import (
	"reflect"
	"testing"
	"time"
)

func TestBackoffDoublesToTenSeconds(t *testing.T) {
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second}
	for n, w := range want {
		if got := Backoff(n); got != w {
			t.Fatalf("Backoff(%d) = %v, want %v", n, got, w)
		}
	}
}

func TestSSHCommandRunsTheRemoteAttach(t *testing.T) {
	got := SSHCommand(Options{Host: "anthony@dev2", SSHArgs: []string{"-p", "2202"}})
	want := []string{"ssh", "-T", "-o", "ServerAliveInterval=10", "-p", "2202", "anthony@dev2", "hqsh", "server", "attach", "main"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q", got)
	}
}
