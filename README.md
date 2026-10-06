# hqsh

A terminal session that survives disconnects, like mosh, but carries the
**raw stream**, so images (Kitty graphics, iTerm2 inline images, sixel) and
every other escape reach your terminal.

```
hqsh dev2                 # connect (or resume) the "main" session on dev2
hqsh dev2 --session work  # another session
hqsh dev2 -- -p 2202      # extra ssh flags
```

Close the laptop, change networks, lose Wi-Fi: when it comes back, hqsh
reconnects and replays exactly the output you missed.

`Ctrl-^ .` detaches (the shell keeps running; `hqsh dev2` picks it up
again). `Ctrl-^ Ctrl-^` sends a literal `Ctrl-^`. When the shell exits,
hqsh exits with its status.

## Install

hqsh must be on both ends: your machine and the host.

```
curl -fsSL https://hqterm.sh/install | sh
```

Or take a binary from [Releases](https://github.com/profullstack/hqsh/releases)
(`hqsh-linux-amd64`, `hqsh-darwin-arm64`, ...), check it against
`SHA256SUMS`, and put it on your PATH as `hqsh`. On the host,
`~/.local/bin/hqsh` works even when that directory is not on the PATH ssh
commands get.

On the host, `hqsh server list` (or `--json`) shows the running sessions.

## Why not mosh?

Mosh keeps its own copy of the screen and syncs only text, so it drops images
in every terminal, by design. Eternal Terminal passes the stream but needs its
own port and daemon. hqsh rides on plain ssh (no new ports, ssh does auth and
encryption) and resumes from a numbered output buffer on the server.

## How it works

```
your terminal ── hqsh ══ ssh ══ hqsh server attach ── unix socket ── hqsh daemon ── PTY ── $SHELL
```

The daemon owns your shell and outlives connections; the client re-runs ssh
with backoff and resumes from the last output it printed. Full detail:
[docs/protocol.md](docs/protocol.md).

## Status

**0.1.0: works.** The client, the PTY daemon and resume are implemented and
tested end to end (a real shell over a pipe in CI, and over ssh by hand):
reconnect after the connection dies, replay of exactly the missed output,
detach and re-attach, exit status, Kitty and iTerm2 image escapes passed
through untouched. The server side runs on Linux and macOS; the client also
builds for Windows. Not yet: local echo prediction, a WebSocket bridge so a
phone/PWA can attach.

Works with any modern terminal: Kitty, Ghostty, WezTerm, Rio, iTerm2,
Windows Terminal. hqtui apps (like `qc`) draw HD emoji through it.

## Build

```
mise install   # Go 1.26
go test ./...
go build ./cmd/hqsh
```

## License

MIT
