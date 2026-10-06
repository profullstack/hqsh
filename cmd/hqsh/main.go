// hqsh: a terminal session that survives disconnects (like mosh) and carries
// the raw stream, so images and every other escape reach your terminal.
//
//	hqsh [user@]host [--session NAME] [-- ssh flags...]
//	hqsh server attach SESSION     (run by the client over ssh)
//	hqsh server daemon SESSION     (started by attach; owns the PTY)
//	hqsh version
package main

import (
	"fmt"
	"os"

	"github.com/profullstack/hqsh/internal/client"
	"github.com/profullstack/hqsh/internal/server"
)

const version = "0.0.1"

const usage = `hqsh: a terminal session that survives disconnects and carries images

Usage:
  hqsh [user@]host [--session NAME] [-- ssh flags...]
  hqsh server attach SESSION
  hqsh server daemon SESSION
  hqsh version
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Println("hqsh", version)
		return nil
	case "server":
		if len(args) < 3 {
			return fmt.Errorf("usage: hqsh server attach|daemon SESSION")
		}
		switch args[1] {
		case "attach":
			return server.Attach(args[2])
		case "daemon":
			return server.Daemon(args[2])
		}
		return fmt.Errorf("unknown server command %q", args[1])
	}
	opts := client.Options{Host: args[0], Session: "main"}
	rest := args[1:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--session", "-s":
			if i+1 >= len(rest) {
				return fmt.Errorf("--session needs a name")
			}
			opts.Session = rest[i+1]
			i++
		case "--":
			opts.SSHArgs = rest[i+1:]
			i = len(rest)
		default:
			return fmt.Errorf("unknown flag %q", rest[i])
		}
	}
	return client.Run(opts)
}
