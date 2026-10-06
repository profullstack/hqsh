//go:build windows

package server

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

// conPTY is a Windows pseudo console (Windows 10 1809 and later): the shell
// writes VT sequences to it like to a Unix PTY, and keystrokes go in as VT.
type conPTY struct {
	hpc  windows.Handle
	in   *os.File // our end of the shell's input
	out  *os.File // our end of the shell's output
	once sync.Once
}

func (c *conPTY) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.in.Write(p) }

func (c *conPTY) Resize(cols, rows uint16) error {
	return windows.ResizePseudoConsole(c.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// Close ends the pseudo console (which ends the shell, if it is still
// there) and our pipe ends. Closing the console is also what lets the last
// read return EOF.
func (c *conPTY) Close() error {
	c.once.Do(func() {
		windows.ClosePseudoConsole(c.hpc)
		c.in.Close()
		c.out.Close()
	})
	return nil
}

// windowsShell picks the shell: $HQSH_SHELL, else PowerShell 7 (pwsh),
// else Windows PowerShell, else %ComSpec% (cmd.exe).
func windowsShell() string {
	if s := os.Getenv("HQSH_SHELL"); s != "" {
		return s
	}
	for _, s := range []string{"pwsh.exe", "powershell.exe"} {
		if p, err := exec.LookPath(s); err == nil {
			return p
		}
	}
	if s := os.Getenv("ComSpec"); s != "" {
		return s
	}
	return `C:\Windows\System32\cmd.exe`
}

func startShell(session, term string, cols, rows uint16) (console, func() int, error) {
	var inR, inW, outR, outW windows.Handle
	if err := windows.CreatePipe(&inR, &inW, nil, 0); err != nil {
		return nil, nil, err
	}
	if err := windows.CreatePipe(&outR, &outW, nil, 0); err != nil {
		windows.CloseHandle(inR)
		windows.CloseHandle(inW)
		return nil, nil, err
	}
	var hpc windows.Handle
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, inR, outW, 0, &hpc); err != nil {
		for _, h := range []windows.Handle{inR, inW, outR, outW} {
			windows.CloseHandle(h)
		}
		return nil, nil, err
	}
	// The pseudo console holds its own copies of these.
	windows.CloseHandle(inR)
	windows.CloseHandle(outW)
	con := &conPTY{hpc: hpc, in: os.NewFile(uintptr(inW), "conpty-in"), out: os.NewFile(uintptr(outR), "conpty-out")}

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		con.Close()
		return nil, nil, err
	}
	defer attrs.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		con.Close()
		return nil, nil, err
	}
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	// No inherited std handles: the console is the shell's only terminal.
	si.Flags = windows.STARTF_USESTDHANDLES

	cmdline, err := windows.UTF16PtrFromString(windows.EscapeArg(windowsShell()))
	if err != nil {
		con.Close()
		return nil, nil, err
	}
	var dir *uint16
	if home, err := os.UserHomeDir(); err == nil {
		dir, _ = windows.UTF16PtrFromString(home)
	}
	env := envBlock(shellEnvWindows(os.Environ(), session))
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(nil, cmdline, nil, nil, false,
		windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_UNICODE_ENVIRONMENT,
		&env[0], dir, &si.StartupInfo, &pi); err != nil {
		con.Close()
		return nil, nil, err
	}
	windows.CloseHandle(pi.Thread)
	return con, func() int {
		defer windows.CloseHandle(pi.Process)
		if _, err := windows.WaitForSingleObject(pi.Process, windows.INFINITE); err != nil {
			return 1
		}
		var code uint32
		if err := windows.GetExitCodeProcess(pi.Process, &code); err != nil {
			return 1
		}
		return int(code)
	}, nil
}

// shellEnvWindows marks the session; TERM means nothing to Windows programs.
func shellEnvWindows(env []string, session string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(strings.ToUpper(kv), "HQSH_SESSION=") {
			out = append(out, kv)
		}
	}
	return append(out, "HQSH_SESSION="+session)
}

// envBlock is CreateProcess's environment: NUL-separated UTF-16, NUL-NUL ended.
func envBlock(env []string) []uint16 {
	var b []uint16
	for _, kv := range env {
		if strings.ContainsRune(kv, 0) {
			continue
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	if len(b) == 0 {
		b = append(b, 0)
	}
	return append(b, 0)
}

func lockSession(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
}

// chmodSocket: Windows has no mode bits for it; the socket lives in the
// user's profile, which only they (and admins) can open.
func chmodSocket(path string) error { return nil }

func isDead(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, os.ErrNotExist)
}
