//go:build !windows

package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
)

// How a session daemon is started. A daemon must outlive the ssh connection
// that started it, so where the OS has a service manager the daemon runs
// under it:
//
//   - systemd (Linux): a transient user unit, hqsh-<session>.service, with
//     lingering enabled so the user's manager (and the session) survives
//     logout. Without linger, logind stops every user unit at the last
//     logout, which is worse than a plain fork, so then we fork instead.
//   - launchd (macOS): a job in the user's launchd domain.
//   - fork: a detached child in its own session (setsid). The fallback
//     everywhere, and what HQSH_SUPERVISOR=fork forces.
//
// A supervisor that fails to start the daemon falls back to fork, so a
// session always starts if it can start at all.
const (
	SupervisorFork    = "fork"
	SupervisorSystemd = "systemd"
	SupervisorLaunchd = "launchd"
)

// Seams for tests.
var (
	runCmd = func(name string, args ...string) ([]byte, error) {
		c := exec.Command(name, args...)
		if name == "systemctl" || name == "systemd-run" {
			c.Env = managerEnv(os.Environ())
		}
		return c.CombinedOutput()
	}
	lookPath  = exec.LookPath
	goos      = runtime.GOOS
	lingerDir = "/var/lib/systemd/linger"
)

// SupervisorReport says which supervisor sessions use on this host, and why.
type SupervisorReport struct {
	Supervisor string
	Reason     string
	Linger     bool // systemd only: lingering is enabled for this user
}

// ChooseSupervisor picks the supervisor for new sessions. HQSH_SUPERVISOR
// (fork, systemd, launchd or auto) overrides the automatic choice.
func ChooseSupervisor() SupervisorReport {
	switch want := os.Getenv("HQSH_SUPERVISOR"); want {
	case SupervisorFork:
		return SupervisorReport{Supervisor: SupervisorFork, Reason: "HQSH_SUPERVISOR=fork"}
	case SupervisorSystemd, SupervisorLaunchd, "", "auto":
		if r, ok := detect(want); ok {
			return r
		} else if want == SupervisorSystemd || want == SupervisorLaunchd {
			return SupervisorReport{Supervisor: SupervisorFork, Reason: fmt.Sprintf("HQSH_SUPERVISOR=%s, but %s", want, r.Reason)}
		} else {
			return SupervisorReport{Supervisor: SupervisorFork, Reason: r.Reason}
		}
	default:
		return SupervisorReport{Supervisor: SupervisorFork, Reason: fmt.Sprintf("HQSH_SUPERVISOR=%q is not fork, systemd, launchd or auto", want)}
	}
}

func detect(want string) (SupervisorReport, bool) {
	switch {
	case want == SupervisorSystemd || (want != SupervisorLaunchd && goos == "linux"):
		return detectSystemd()
	case want == SupervisorLaunchd || goos == "darwin":
		return detectLaunchd()
	}
	return SupervisorReport{Reason: "no service manager on " + goos}, false
}

func detectSystemd() (SupervisorReport, bool) {
	if _, err := lookPath("systemd-run"); err != nil {
		return SupervisorReport{Reason: "systemd-run is not installed"}, false
	}
	if _, err := runCmd("systemctl", "--user", "show-environment"); err != nil {
		return SupervisorReport{Reason: "no systemd user manager is reachable (systemctl --user failed)"}, false
	}
	if !lingering() {
		return SupervisorReport{Reason: "systemd is here, but lingering is off for this user, so sessions would stop at logout; run `hqsh server setup`"}, false
	}
	return SupervisorReport{Supervisor: SupervisorSystemd, Reason: "systemd user manager with lingering enabled", Linger: true}, true
}

func detectLaunchd() (SupervisorReport, bool) {
	if _, err := lookPath("launchctl"); err != nil {
		return SupervisorReport{Reason: "launchctl is not installed"}, false
	}
	return SupervisorReport{Supervisor: SupervisorLaunchd, Reason: "launchd, domain " + launchdDomain()}, true
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

func lingering() bool {
	name := currentUser()
	if name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(lingerDir, name))
	return err == nil
}

// EnableLinger turns on lingering for the current user (logind allows that
// for yourself on most distributions; root can do it for anyone with
// `loginctl enable-linger USER`).
func EnableLinger() error {
	if lingering() {
		return nil
	}
	out, err := runCmd("loginctl", "enable-linger")
	if err != nil {
		return fmt.Errorf("loginctl enable-linger: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if !lingering() {
		return errors.New("loginctl enable-linger ran, but lingering is still off")
	}
	return nil
}

// Setup reports how sessions start on this host and, unless check is set,
// fixes what it can: on systemd hosts that means enabling lingering, without
// which sessions would have to fall back to plain forks. It returns an error
// only when it was asked to fix something and could not.
func Setup(w io.Writer, check bool) error {
	r := ChooseSupervisor()
	if r.Supervisor == SupervisorFork && goos == "linux" && os.Getenv("HQSH_SUPERVISOR") != SupervisorFork {
		if _, err := lookPath("systemd-run"); err == nil {
			if _, err := runCmd("systemctl", "--user", "show-environment"); err == nil && !lingering() {
				if check {
					fmt.Fprintln(w, "lingering: off (run `hqsh server setup` to turn it on)")
				} else if err := EnableLinger(); err != nil {
					fmt.Fprintf(w, "supervisor: fork (%s)\n", r.Reason)
					return fmt.Errorf("hqsh: could not enable lingering (%v); ask root to run: loginctl enable-linger %s", err, currentUser())
				} else {
					fmt.Fprintln(w, "lingering: enabled")
					r = ChooseSupervisor()
				}
			}
		}
	}
	fmt.Fprintf(w, "supervisor: %s (%s)\n", r.Supervisor, r.Reason)
	if dir, err := SocketDir(); err == nil {
		fmt.Fprintf(w, "sockets: %s\n", dir)
	}
	switch r.Supervisor {
	case SupervisorSystemd:
		fmt.Fprintln(w, "sessions run as hqsh-<session>.service: systemctl --user status 'hqsh-*', journalctl --user -u 'hqsh-*'")
	case SupervisorLaunchd:
		fmt.Fprintf(w, "sessions run as launchd jobs %s.<session>: launchctl print %s/%s.main\n", "sh.hqterm.hqsh", launchdDomain(), "sh.hqterm.hqsh")
	}
	return nil
}

// startDaemon starts the session's daemon under the chosen supervisor,
// falling back to a plain fork if the supervisor refuses.
func startDaemon(session string) error {
	cmd, err := daemonCommand(session)
	if err != nil {
		return err
	}
	switch ChooseSupervisor().Supervisor {
	case SupervisorSystemd:
		if err := startSystemd(session, cmd); err == nil {
			return nil
		}
	case SupervisorLaunchd:
		if err := startLaunchd(session, cmd); err == nil {
			return nil
		}
	}
	return startFork(cmd)
}

func startFork(cmd *exec.Cmd) error {
	// Its own session: no controlling terminal, and the ssh hangup that
	// ends this attach never reaches it. stdio stays nil, i.e. /dev/null.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hqsh: starting the daemon: %w", err)
	}
	// Reap it if it exits while we live (a daemon that loses a start race
	// exits at once).
	go func() { _ = cmd.Wait() }()
	return nil
}

// UnitName is the systemd unit for a session: hqsh-<session>.service, with
// anything outside [A-Za-z0-9_-] hex-escaped the way systemd-escape does.
func UnitName(session string) string {
	var b strings.Builder
	b.WriteString("hqsh-")
	for i := 0; i < len(session); i++ {
		c := session[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	b.WriteString(".service")
	return b.String()
}

// passEnv: what a daemon started by a service manager needs from the
// environment attach ran in. The manager's own environment has HOME, USER,
// SHELL and XDG_RUNTIME_DIR; the rest of the shell's world comes from its
// login profile (the shell runs with -l).
func passEnv(environ []string) []string {
	var out []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case k == "PATH", k == "LANG", k == "LANGUAGE", k == "TZ", k == "TERMINFO", k == "TERMINFO_DIRS",
			strings.HasPrefix(k, "LC_"), strings.HasPrefix(k, "HQSH_"):
			out = append(out, kv)
		}
	}
	return out
}

// managerEnv points systemctl and systemd-run at the user's real manager.
// They find it through XDG_RUNTIME_DIR, which is also where hqsh keeps its
// sockets; the two are the same in an ssh login, but when they differ the
// manager lives in /run/user/UID. (The unit itself is handed hqsh's
// XDG_RUNTIME_DIR, so its socket lands where attach dials.)
func managerEnv(environ []string) []string {
	real := fmt.Sprintf("/run/user/%d", os.Getuid())
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return environ
	}
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, "XDG_RUNTIME_DIR=") {
			out = append(out, kv)
		}
	}
	return append(out, "XDG_RUNTIME_DIR="+real)
}

// SystemdRunArgs is the systemd-run command line for a session's daemon.
func SystemdRunArgs(session string, argv, env []string) []string {
	args := []string{"--user", "--quiet", "--collect",
		"--unit=" + UnitName(session),
		"--description=hqsh session " + session,
		"--property=Type=simple",
	}
	for _, kv := range env {
		args = append(args, "--setenv="+kv)
	}
	return append(append(args, "--"), argv...)
}

func startSystemd(session string, cmd *exec.Cmd) error {
	env := passEnv(os.Environ())
	if cmd.Env != nil {
		env = append(env, passEnv(cmd.Env)...)
	}
	// The daemon mirrors the attaching side: same socket dir, shell and home.
	for _, k := range []string{"XDG_RUNTIME_DIR", "HOME", "SHELL"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	out, err := runCmd("systemd-run", SystemdRunArgs(session, argv, env)...)
	if err != nil {
		// A unit by that name already running means a daemon is coming up
		// (or up) for this session: dial it.
		if strings.Contains(string(out), "already") {
			return nil
		}
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ---------------------------------------------------------------- launchd ---

// launchdDomains are the domains a session job may go in, best first: the
// GUI login's when the user has one (it is where their own agents run),
// else the per-user background domain, which exists for ssh-only users too.
// Both outlive the ssh connection.
func launchdDomains() []string {
	gui := fmt.Sprintf("gui/%d", os.Getuid())
	user := fmt.Sprintf("user/%d", os.Getuid())
	if _, err := runCmd("launchctl", "print", gui); err == nil {
		return []string{gui, user}
	}
	return []string{user}
}

func launchdDomain() string { return launchdDomains()[0] }

// LaunchdLabel is the launchd job label for a session.
func LaunchdLabel(session string) string {
	return "sh.hqterm.hqsh." + strings.TrimSuffix(strings.TrimPrefix(UnitName(session), "hqsh-"), ".service")
}

func launchdPlist(label string, argv, env []string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return r.Replace(s)
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + esc(label) + `</string>
	<key>ProgramArguments</key>
	<array>
`)
	for _, a := range argv {
		b.WriteString("\t\t<string>" + esc(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n")
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		b.WriteString("\t\t<key>" + esc(k) + "</key><string>" + esc(v) + "</string>\n")
	}
	b.WriteString(`	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><false/>
	<key>ProcessType</key><string>Interactive</string>
	<key>StandardInPath</key><string>/dev/null</string>
	<key>StandardOutPath</key><string>/dev/null</string>
	<key>StandardErrorPath</key><string>/dev/null</string>
</dict>
</plist>
`)
	return b.String()
}

func startLaunchd(session string, cmd *exec.Cmd) error {
	label := LaunchdLabel(session)
	dir, err := SocketDir()
	if err != nil {
		return err
	}
	// Not ~/Library/LaunchAgents: a job there would also start at every login.
	if err := ensureDir(dir); err != nil {
		return err
	}
	plist := filepath.Join(dir, label+".plist")
	env := passEnv(os.Environ())
	if cmd.Env != nil {
		env = append(env, passEnv(cmd.Env)...)
	}
	for _, k := range []string{"HOME", "USER", "SHELL", "XDG_RUNTIME_DIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	if err := os.WriteFile(plist, []byte(launchdPlist(label, argv, env)), 0o600); err != nil {
		return err
	}
	var errs []string
	for _, domain := range launchdDomains() {
		// A finished job stays loaded; unload the old one so the new one runs.
		_, _ = runCmd("launchctl", "bootout", domain+"/"+label)
		out, err := runCmd("launchctl", "bootstrap", domain, plist)
		if err == nil {
			launchdUsed.Store(session, domain)
			return nil
		}
		errs = append(errs, fmt.Sprintf("%s: %v: %s", domain, err, strings.TrimSpace(string(out))))
	}
	return fmt.Errorf("launchctl bootstrap: %s", strings.Join(errs, "; "))
}

// launchdUsed remembers the domain each session's job went into.
var launchdUsed sync.Map
