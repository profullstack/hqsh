// hqsh: a terminal session that survives disconnects (like mosh) and carries
// the raw stream, so images and every other escape reach your terminal.
//
//	hqsh [user@]host [--session NAME] [--steal | --read-only] [--tailscale auto|on|off] [-- ssh flags...]
//	hqsh server attach SESSION     (run by the client over ssh)
//	hqsh server daemon SESSION     (started by attach; owns the PTY)
//	hqsh server list [--json]      (live sessions on this host)
//	hqsh server setup [--check]    (how sessions start here; enables linger)
//	hqsh version
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/profullstack/hqsh/internal/client"
	"github.com/profullstack/hqsh/internal/server"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = "0.2.0"

const usage = `hqsh: a terminal session that survives disconnects and carries images

Usage:
  hqsh [user@]host [--session NAME] [--steal | --read-only] [--tailscale MODE] [-- ssh flags...]
  hqsh server attach SESSION
  hqsh server daemon SESSION
  hqsh server list [--json]
  hqsh server setup [--check]
  hqsh version

On the host, each session's daemon runs under the service manager: a
systemd user unit hqsh-<session>.service on Linux (with lingering, so it
survives logout; "hqsh server setup" turns that on), a launchd job on
macOS, a process outside the ssh job on a ConPTY on Windows, else a
detached process. HQSH_SUPERVISOR=fork|systemd|launchd|auto overrides the
choice; HQSH_SHELL picks the session's shell.

Several clients can attach to one session at once, like tmux: all of them
see the output, any of them can type, and the window is the smallest of
their sizes.
  -s, --session NAME  the session (default "main")
  -d, --steal         detach every other client first (tmux attach -d)
  -r, --read-only     watch only; your keys are not sent
  -t, --tailscale M   auto (default; $HQSH_TAILSCALE), on or off: when
                      Tailscale runs here and the host is an online peer,
                      connect over its tailnet address (ssh config still
                      supplies user, port and keys); falls back to the
                      normal route if that fails, unless "on"

Keys: Ctrl-^ then .  detach (the session keeps running)
      Ctrl-^ Ctrl-^  send a literal Ctrl-^
`

func main() {
	code, err := run(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func run(args []string) (int, error) {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return 0, nil
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("hqsh", version)
		return 0, nil
	case "server":
		if len(args) >= 2 && args[1] == "setup" {
			check := len(args) >= 3 && args[2] == "--check"
			if err := server.Setup(os.Stdout, check); err != nil {
				return 1, err
			}
			return 0, nil
		}
		if len(args) >= 2 && args[1] == "list" {
			asJSON := len(args) >= 3 && args[2] == "--json"
			sessions, err := server.List()
			if err != nil {
				return 1, err
			}
			return 0, server.WriteList(os.Stdout, sessions, asJSON)
		}
		if len(args) < 3 {
			return 2, fmt.Errorf("usage: hqsh server attach|daemon SESSION, or hqsh server list [--json]")
		}
		switch args[1] {
		case "attach":
			return 0, server.Attach(args[2])
		case "daemon":
			return 0, server.Daemon(args[2])
		}
		return 2, fmt.Errorf("unknown server command %q", args[1])
	}
	opts := client.Options{Host: args[0], Session: "main"}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		if v, ok := strings.CutPrefix(rest[i], "--tailscale="); ok {
			opts.Tailscale = v
			continue
		}
		switch rest[i] {
		case "--session", "-s":
			if i+1 >= len(rest) {
				return 2, fmt.Errorf("--session needs a name")
			}
			opts.Session = rest[i+1]
			i++
		case "--steal", "-d":
			opts.Steal = true
		case "--read-only", "-r":
			opts.ReadOnly = true
		case "--tailscale", "-t":
			if i+1 >= len(rest) {
				return 2, fmt.Errorf("--tailscale needs auto, on or off")
			}
			opts.Tailscale = rest[i+1]
			i++
		case "--":
			opts.SSHArgs = rest[i+1:]
			i = len(rest)
		default:
			return 2, fmt.Errorf("unknown flag %q", rest[i])
		}
	}
	if err := server.ValidSession(opts.Session); err != nil {
		return 2, err
	}
	return client.Run(opts)
}
