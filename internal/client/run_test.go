package client

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/profullstack/hqsh/internal/proto"
)

type pipeConn struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error                { p.w.Close(); p.r.Close(); return nil }

// fakeServer welcomes the client and reports every INPUT it gets.
func fakeServer(t *testing.T, inputs chan<- []byte, hellos chan<- proto.HelloMsg) func() (io.ReadWriteCloser, error) {
	return func() (io.ReadWriteCloser, error) {
		cr, sw := io.Pipe()
		sr, cw := io.Pipe()
		go func() {
			defer sw.Close()
			f, err := proto.Read(sr)
			if err != nil || f.Type != proto.Hello {
				return
			}
			h, _ := proto.DecodeHello(f.Payload)
			hellos <- h
			proto.Write(sw, proto.Frame{Type: proto.Welcome, Payload: proto.WelcomeMsg{Version: proto.Version, FirstSeq: 1}.Encode()})
			proto.Write(sw, proto.Frame{Type: proto.Output, Payload: proto.OutputMsg{Seq: 1, Data: []byte("ready")}.Encode()})
			for {
				f, err := proto.Read(sr)
				if err != nil {
					return
				}
				switch f.Type {
				case proto.Input:
					inputs <- f.Payload
				case proto.Ping:
					proto.Write(sw, proto.Frame{Type: proto.Pong, Payload: f.Payload})
				}
			}
		}()
		return &pipeConn{r: cr, w: cw}, nil
	}
}

func TestEscapeKeys(t *testing.T) {
	inputs := make(chan []byte, 16)
	hellos := make(chan proto.HelloMsg, 1)
	keys, typed := io.Pipe()
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		code, err := Run(Options{Host: "h", Session: "s", Term: "xterm-kitty", Stdin: keys, Stdout: &out, Stderr: io.Discard,
			Dial: fakeServer(t, inputs, hellos)})
		if err == nil && code != 0 {
			err = errors.New("nonzero exit")
		}
		done <- err
	}()
	h := <-hellos
	if h.Session != "s" || h.Term != "xterm-kitty" || h.LastSeq != 0 {
		t.Fatalf("hello %+v", h)
	}
	// a, a doubled Ctrl-^ (one literal), b, Ctrl-^ x (passed through), then detach.
	io.WriteString(typed, "a\x1e\x1eb\x1exc")
	var got []byte
	for !bytes.Equal(got, []byte("a\x1eb\x1exc")) {
		select {
		case b := <-inputs:
			got = append(got, b...)
		case <-time.After(5 * time.Second):
			t.Fatalf("inputs so far %q", got)
		}
	}
	io.WriteString(typed, "\x1e.")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl-^ . did not detach")
	}
	if !strings.Contains(out.String(), "ready") {
		t.Fatalf("output %q", out.String())
	}
}

// With no hqsh on the host (the remote shell exits 127), the client tries
// ~/.local/bin/hqsh too, then says how to install it.
func TestMissingRemoteSaysHowToInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake ssh")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\necho 'sh: hqsh: not found' >&2\nexit 127\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Run(Options{Host: "box", Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	var nr *NoRemoteError
	if !errors.As(err, &nr) || !strings.Contains(err.Error(), "curl -fsSL https://hqterm.sh/install | sh") {
		t.Fatalf("err = %v", err)
	}
	calls, _ := os.ReadFile(log)
	want := "-T -o ServerAliveInterval=10 box hqsh server attach main\n-T -o ServerAliveInterval=10 box ~/.local/bin/hqsh server attach main\n"
	if string(calls) != want {
		t.Fatalf("ssh calls:\n%s", calls)
	}
}

func TestUnreachableHostFailsFast(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as a fake ssh")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'ssh: connect to host box port 22: Connection refused' >&2\nexit 255\n"
	os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err := Run(Options{Host: "box", Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "Connection refused") {
		t.Fatalf("err = %v", err)
	}
}
