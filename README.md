# hqsh

A terminal session that survives disconnects, like mosh, but carries the
**raw stream**, so images (Kitty graphics, iTerm2 inline images, sixel) and
every other escape reach your terminal.

```
hqsh dev2                 # connect (or resume) the "main" session on dev2
hqsh dev2 --session work  # another session
hqsh dev2 -- -p 2202      # extra ssh flags
hqsh dev2 --read-only     # watch the session someone else is driving
hqsh dev2 --steal         # take it over: everyone else is detached
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

On the host, `hqsh server list` (or `--json`) shows the running sessions
and how many clients each has.

## Several clients, like tmux

Attach to the same session from a second terminal, laptop or person, and
both are live at once, the way `tmux attach` works:

| | tmux | hqsh |
|---|---|---|
| Sessions | `tmux new -s work`, `tmux attach -t work` | `hqsh host --session work` (created on first attach) |
| Shared attach | every client sees the output, any can type | the same (the default since 0.2.0) |
| Window size | smallest client's cols and rows | the same; bigger clients see it top-left |
| Take over | `tmux attach -d` | `hqsh host --steal` (`-d`): the others are told and exit |
| Watch only | `tmux attach -r` | `hqsh host --read-only` (`-r`): your keys are not sent, and the server ignores them anyway |
| List | `tmux ls` | `hqsh server list [--json]` on the host |
| Detach | `Ctrl-b d` | `Ctrl-^ .` |

Unlike tmux there are no windows or panes (run tmux or zellij inside hqsh
for those), and hqsh carries the raw stream, so images still work. Nobody
is notified when someone else attaches. A slow client never stalls the
others: it falls behind and, past the 4 MiB buffer, is dropped and resumes
by itself. Details: [docs/protocol.md](docs/protocol.md#shared-attach).

Sessions started by hqsh 0.1.x keep the old take-over behaviour until they
end; attach again after `exit` to get a 0.2.0 daemon.

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

**0.2.0: works.** The client, the PTY daemon and resume are implemented and
tested end to end (a real shell over a pipe in CI, and over ssh by hand):
reconnect after the connection dies, replay of exactly the missed output,
detach and re-attach, exit status, Kitty and iTerm2 image escapes passed
through untouched. 0.2.0 adds shared attach (several clients on one
session), `--steal` and `--read-only`. The server side runs on Linux and macOS; the client also
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
