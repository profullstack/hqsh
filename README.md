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

**0.0.1: stub.** The wire format and the resume buffer are written and
tested; the client loop and the PTY daemon are outlined (`TODO`s in
`internal/client` and `internal/server`). Next: the daemon, then the client,
then a WebSocket bridge so a phone/PWA can attach.

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
