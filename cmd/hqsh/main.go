// hqsh: a terminal session that survives disconnects (like mosh) and carries
// the raw stream, so images and every other escape reach your terminal.
//
//	hqsh [user@]host [--session NAME] [-- ssh flags...]
//	hqsh server attach SESSION     (run by the client over ssh)
//	hqsh server daemon SESSION     (started by attach; owns the PTY)
//	hqsh server list [--json]      (live sessions on this host)
//	hqsh version
package main

import (
	"fmt"
	"os"

	"github.com/profullstack/hqsh/internal/client"
	"github.com/profullstack/hqsh/internal/server"
)

// version is set by the release build (-ldflags "-X main.version=...").
var version = "0.1.0"

const usage = `hqsh: a terminal session that survives disconnects and carries images

Usage:
  hqsh [user@]host [--session NAME] [-- ssh flags...]
  hqsh server attach SESSION
  hqsh server daemon SESSION
  hqsh server list [--json]
  hqsh version

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
		switch rest[i] {
		case "--session", "-s":
			if i+1 >= len(rest) {
				return 2, fmt.Errorf("--session needs a name")
			}
			opts.Session = rest[i+1]
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
