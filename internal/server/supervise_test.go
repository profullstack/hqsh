//go:build !windows

package server

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/profullstack/hqsh/internal/proto"
)

// fakeHost stubs the commands and files supervisor detection looks at.
func fakeHost(t *testing.T, os_ string, have map[string]bool, managerUp, linger bool) *[]string {
	t.Helper()
	calls := &[]string{}
	oldRun, oldLook, oldGoos, oldLinger := runCmd, lookPath, goos, lingerDir
	t.Cleanup(func() { runCmd, lookPath, goos, lingerDir = oldRun, oldLook, oldGoos, oldLinger })
	goos = os_
	lingerDir = t.TempDir()
	if linger {
		if err := os.WriteFile(filepath.Join(lingerDir, currentUser()), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lookPath = func(name string) (string, error) {
		if have[name] {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
	runCmd = func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name == "systemctl" && !managerUp {
			return []byte("Failed to connect to bus"), errors.New("exit 1")
		}
		return nil, nil
	}
	return calls
}

func TestChooseSupervisor(t *testing.T) {
	sd := map[string]bool{"systemd-run": true}
	cases := []struct {
		name, env, goos string
		have            map[string]bool
		managerUp       bool
		linger          bool
		want            string
		reason          string
	}{
		{"linux with systemd and linger", "", "linux", sd, true, true, SupervisorSystemd, "lingering enabled"},
		{"linux without linger forks", "", "linux", sd, true, false, SupervisorFork, "hqsh server setup"},
		{"linux without a user manager forks", "", "linux", sd, false, true, SupervisorFork, "systemctl --user failed"},
		{"linux without systemd forks", "", "linux", nil, true, true, SupervisorFork, "systemd-run is not installed"},
		{"macOS uses launchd", "", "darwin", map[string]bool{"launchctl": true}, false, false, SupervisorLaunchd, "launchd"},
		{"BSD forks", "", "freebsd", nil, false, false, SupervisorFork, "no service manager"},
		{"forced fork", "fork", "linux", sd, true, true, SupervisorFork, "HQSH_SUPERVISOR=fork"},
		{"forced systemd that is not there", "systemd", "linux", nil, true, true, SupervisorFork, "HQSH_SUPERVISOR=systemd, but"},
		{"nonsense", "upstart", "linux", sd, true, true, SupervisorFork, "is not fork"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fakeHost(t, c.goos, c.have, c.managerUp, c.linger)
			t.Setenv("HQSH_SUPERVISOR", c.env)
			r := ChooseSupervisor()
			if r.Supervisor != c.want || !strings.Contains(r.Reason, c.reason) {
				t.Fatalf("got %+v, want %s (%q)", r, c.want, c.reason)
			}
		})
	}
}

func TestEnableLingerCallsLoginctlOnlyWhenOff(t *testing.T) {
	calls := fakeHost(t, "linux", nil, true, true)
	if err := EnableLinger(); err != nil || len(*calls) != 0 {
		t.Fatalf("already lingering: err=%v calls=%v", err, *calls)
	}
	calls = fakeHost(t, "linux", nil, true, false)
	if err := EnableLinger(); err == nil {
		t.Fatal("linger still off after the (fake) loginctl must be an error")
	}
	if len(*calls) != 1 || (*calls)[0] != "loginctl enable-linger" {
		t.Fatalf("calls %v", *calls)
	}
}

func TestUnitNameEscapes(t *testing.T) {
	for in, want := range map[string]string{
		"main":   "hqsh-main.service",
		"main-2": "hqsh-main-2.service",
		"a b":    `hqsh-a\x20b.service`,
		"café":   `hqsh-caf\xc3\xa9.service`,
		"x.y":    `hqsh-x\x2ey.service`,
	} {
		if got := UnitName(in); got != want {
			t.Errorf("UnitName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := LaunchdLabel("main"); got != "sh.hqterm.hqsh.main" {
		t.Errorf("LaunchdLabel = %q", got)
	}
}

func TestSystemdRunArgs(t *testing.T) {
	got := strings.Join(SystemdRunArgs("main", []string{"/home/a/.local/bin/hqsh", "server", "daemon", "main"}, []string{"LANG=C.UTF-8"}), " ")
	want := "--user --quiet --collect --unit=hqsh-main.service --description=hqsh session main --property=Type=simple --setenv=LANG=C.UTF-8 -- /home/a/.local/bin/hqsh server daemon main"
	if got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
}

func TestPassEnvKeepsOnlyWhatTheShellNeeds(t *testing.T) {
	got := passEnv([]string{"PATH=/bin", "LANG=en_US.UTF-8", "LC_ALL=C", "SSH_AUTH_SOCK=/tmp/x", "DISPLAY=:0", "HQSH_X=1", "TZ=UTC", "broken"})
	if strings.Join(got, " ") != "PATH=/bin LANG=en_US.UTF-8 LC_ALL=C HQSH_X=1 TZ=UTC" {
		t.Fatalf("passEnv = %v", got)
	}
}

func TestLaunchdPlistEscapes(t *testing.T) {
	p := launchdPlist("sh.hqterm.hqsh.main", []string{"/a b/hqsh", "server", "daemon", "<x>"}, []string{"K=v&w"})
	for _, want := range []string{"<string>/a b/hqsh</string>", "<string>&lt;x&gt;</string>", "<key>K</key><string>v&amp;w</string>", "<key>KeepAlive</key><false/>"} {
		if !strings.Contains(p, want) {
			t.Errorf("plist lacks %q:\n%s", want, p)
		}
	}
}

// The real service manager, where the machine has one: the session daemon
// runs as a systemd unit (Linux) or launchd job (macOS) and still serves an
// attach. CI's macOS runner exercises launchd; a Linux box with a lingering
// user manager exercises systemd. Elsewhere it skips.
func TestSessionRunsUnderTheRealServiceManager(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	t.Setenv("HQSH_SUPERVISOR", "auto")
	r := ChooseSupervisor()
	if r.Supervisor == SupervisorFork {
		t.Skipf("no service manager here: %s", r.Reason)
	}
	sandbox(t)
	const session = "svc-real"
	c, w := dialPeer(t, session, 0)
	if w.Gap() {
		t.Fatal("fresh session reported a gap")
	}
	c.until("$ ")
	switch r.Supervisor {
	case SupervisorSystemd:
		out, err := runCmd("systemctl", "--user", "is-active", UnitName(session))
		if err != nil || strings.TrimSpace(string(out)) != "active" {
			t.Fatalf("unit %s not active: %v %s", UnitName(session), err, out)
		}
	case SupervisorLaunchd:
		out, err := runCmd("launchctl", "print", launchdDomain()+"/"+LaunchdLabel(session))
		if err != nil || !strings.Contains(string(out), "state = running") {
			t.Fatalf("launchd job not running: %v\n%s", err, out)
		}
	}
	c.send(proto.Input, []byte("echo under-"+r.Supervisor+"\n"))
	c.until("\nunder-" + r.Supervisor + "\r\n")
	c.send(proto.Input, []byte("exit 0\n"))
	for f := c.next(); f.Type != proto.Exit; f = c.next() {
	}
}
