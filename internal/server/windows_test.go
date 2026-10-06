//go:build windows

package server

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/profullstack/hqsh/internal/proto"
	"golang.org/x/sys/windows"
)

// As on Unix, attach re-execs this test binary as the daemon.
func TestMain(m *testing.M) {
	if os.Getenv("HQSH_TEST_DAEMON") == "1" {
		if err := Daemon(os.Getenv("HQSH_TEST_SESSION")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Setenv("HQSH_SUPERVISOR", SupervisorFork)
	daemonCommand = func(session string) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "HQSH_TEST_DAEMON=1", "HQSH_TEST_SESSION="+session)
		return cmd, nil
	}
	os.Exit(m.Run())
}

func winSandbox(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hqsh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("HQSH_SHELL", os.Getenv("ComSpec"))
}

// A cmd.exe session on a real ConPTY: it starts, computes, resizes, survives
// a disconnect, is listed, and its exit status comes back.
func TestWindowsSessionOnConPTY(t *testing.T) {
	winSandbox(t)
	const session = "win"
	c1, w := dialPeer(t, session, 0)
	if w.Gap() {
		t.Fatal("fresh session reported a gap")
	}
	c1.until(">") // the prompt
	c1.send(proto.Input, []byte("set /a 6*7\r"))
	c1.until("42")
	c1.send(proto.Resize, proto.EncodeResize(100, 30))
	if found, attached := listed(t, session); !found || !attached {
		t.Fatalf("list while attached: found=%v attached=%v", found, attached)
	}
	last := c1.last()
	c1.c.Close()

	c2, w := dialPeer(t, session, last)
	if w.Gap() {
		t.Fatalf("resume reported a gap: %+v", w)
	}
	c2.send(proto.Input, []byte("set /a 7*8\r"))
	c2.until("56")
	c2.send(proto.Input, []byte("exit 7\r"))
	for {
		f := c2.next()
		if f.Type == proto.Exit {
			code, _ := proto.DecodeExit(f.Payload)
			if code != 7 {
				t.Fatalf("exit status %d, want 7", code)
			}
			return
		}
	}
}

func TestWMICommandLineQuotes(t *testing.T) {
	got := WMICommandLine([]string{`C:\Program Files\hqsh\hqsh.exe`, "server", "daemon", "main"})
	if got != `"C:\Program Files\hqsh\hqsh.exe" server daemon main` {
		t.Fatalf("got %s", got)
	}
}

func TestEnvBlockIsDoubleNulTerminated(t *testing.T) {
	b := envBlock([]string{"A=1", "B=2"})
	if s := windows.UTF16ToString(b); s != "A=1" {
		t.Fatalf("first entry %q", s)
	}
	if b[len(b)-1] != 0 || b[len(b)-2] != 0 {
		t.Fatal("not NUL-NUL terminated")
	}
	if n := strings.Count(string(windowsUTF16(b)), "\x00"); n != 3 {
		t.Fatalf("%d NULs, want 3", n)
	}
}

func windowsUTF16(b []uint16) []rune {
	r := make([]rune, len(b))
	for i, u := range b {
		r[i] = rune(u)
	}
	return r
}

// The WMI path (what runs where the ssh job refuses breakaway, as on CI)
// starts a process outside our job. A harmless command: a WMI-started
// process gets the user's default environment, not this test's.
func TestWMIStartsAProcess(t *testing.T) {
	cmd := exec.Command(os.Getenv("ComSpec"), "/c", "exit", "0")
	if err := startWMI(cmd); err != nil {
		t.Fatalf("Win32_Process.Create: %v", err)
	}
}

// Whether this machine's job lets the daemon break away; informational (CI
// runners differ), the start must work either way.
func TestDaemonStartsWithoutTheForkOverride(t *testing.T) {
	winSandbox(t)
	t.Setenv("HQSH_SUPERVISOR", SupervisorBreakaway)
	cmd, _ := daemonCommand("probe")
	if err := startDetached(cmd, detachFlags|windows.CREATE_BREAKAWAY_FROM_JOB); err != nil {
		t.Logf("breakaway refused here (%v); sessions use WMI", err)
	} else {
		t.Log("breakaway allowed here")
	}
}
