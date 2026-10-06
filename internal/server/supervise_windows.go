//go:build windows

package server

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// On Windows the session daemon has to escape the ssh connection's job
// object: Win32-OpenSSH runs each connection in a job that kills everything
// in it when the connection closes. In order of preference:
//
//   - breakaway: CREATE_BREAKAWAY_FROM_JOB, when the job allows it;
//   - wmi: Win32_Process.Create, whose processes are created by the WMI
//     service and so belong to no ssh job;
//   - detached: a plain detached process, which lives only as long as the
//     connection's job does (still fine for a session you stay attached to).
//
// HQSH_SUPERVISOR=fork forces the plain detached process.
const (
	SupervisorFork      = "fork"
	SupervisorBreakaway = "breakaway"
	SupervisorWMI       = "wmi"
)

const detachFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP

func startDaemon(session string) error {
	cmd, err := daemonCommand(session)
	if err != nil {
		return err
	}
	if os.Getenv("HQSH_SUPERVISOR") != SupervisorFork {
		if err := startDetached(cmd, detachFlags|windows.CREATE_BREAKAWAY_FROM_JOB); err == nil {
			return nil
		}
		if err := startWMI(cmd); err == nil {
			return nil
		}
		// Rebuild: an exec.Cmd cannot be started twice.
		if cmd, err = daemonCommand(session); err != nil {
			return err
		}
	}
	return startDetached(cmd, detachFlags)
}

func startDetached(cmd *exec.Cmd, flags uint32) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags, HideWindow: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("hqsh: starting the daemon: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// WMICommandLine is the command line handed to Win32_Process.Create.
func WMICommandLine(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = windows.EscapeArg(a)
	}
	return strings.Join(q, " ")
}

// startWMI creates the daemon through WMI. The new process gets the user's
// default environment rather than ours, which is fine for a real daemon:
// its socket directory comes from the profile, as attach's does.
func startWMI(cmd *exec.Cmd) error {
	line := WMICommandLine(append([]string{cmd.Path}, cmd.Args[1:]...))
	ps := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command",
		"$r = Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$env:HQSH_WMI_CMDLINE}; exit [int]$r.ReturnValue")
	ps.Env = append(os.Environ(), "HQSH_WMI_CMDLINE="+line)
	ps.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := ps.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Win32_Process.Create: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Setup reports how sessions start on this Windows host. There is nothing to
// enable: no service, port or scheduled task.
func Setup(w io.Writer, check bool) error {
	how := "breakaway from the ssh job, else WMI (Win32_Process.Create), else a detached process"
	if os.Getenv("HQSH_SUPERVISOR") == SupervisorFork {
		how = "a detached process (HQSH_SUPERVISOR=fork)"
	}
	fmt.Fprintf(w, "supervisor: %s\n", how)
	fmt.Fprintf(w, "shell: %s (HQSH_SHELL overrides)\n", windowsShell())
	if dir, err := SocketDir(); err == nil {
		fmt.Fprintf(w, "sockets: %s\n", dir)
	}
	return nil
}
