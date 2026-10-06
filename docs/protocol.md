# hqsh protocol (v1)

hqsh keeps a terminal session alive on a server, the way mosh does, but it
carries the raw byte stream instead of a re-rendered screen. So everything a
program writes reaches your terminal, including images (Kitty graphics,
iTerm2 inline images, sixel).

## Shape

```
your terminal ── hqsh (client) ══ ssh ══ hqsh server attach ── unix socket ── hqsh daemon ── PTY ── $SHELL
```

- **Transport:** the ssh connection's stdin and stdout. No new ports and no
  firewall changes; ssh does the auth and encryption.
- **Daemon:** one per session per user. It owns the PTY and outlives any
  connection. Its socket is `$XDG_RUNTIME_DIR/hqsh/<session>.sock` (or
  `~/.local/state/hqsh/` when there is no runtime dir), mode 0600.
- **Attach:** `hqsh server attach <session>` connects stdio to the daemon,
  starting the daemon first if needed (`hqsh server daemon <session>`,
  detached with `setsid`, stdio on `/dev/null`; attach waits up to 3 s for
  its socket). A newer attach takes over from an older one. A lock file next
  to the socket keeps it to one daemon per session.
- **Shell:** the daemon starts `$SHELL -l` (or `/bin/sh -l`) in a PTY on the
  first `HELLO`, with that client's size and `TERM` (falling back to
  `xterm-256color` when the host has no terminfo entry for it) and
  `HQSH_SESSION=<session>`. When the shell exits the daemon sends `EXIT`,
  removes its socket and quits.
- **Finding hqsh:** the client runs `ssh -T host hqsh server attach <session>`;
  when that exits 127 (not on the non-interactive PATH) it retries with
  `~/.local/bin/hqsh`, and failing that tells you to install it with
  `curl -fsSL https://hqterm.sh/install | sh`.
- **Listing:** `hqsh server list [--json]` prints the live sessions; `--json`
  is an array of `{"name": "main", "attached": true}` (`[]` when none).
  Sockets nobody answers on are removed.

## Frames

Every frame is `type (1 byte) | length (4 bytes, big-endian) | payload`.
Payloads are capped at 1 MiB.

| Type | Name | Direction | Payload |
|---|---|---|---|
| 1 | `HELLO` | c→s | version u16, last seq u64, cols u16, rows u16, session name, then optionally a 0 byte and the client's `TERM` |
| 2 | `WELCOME` | s→c | version u16, first seq the server will send u64, flags u8 (bit0: gap, the client missed output that is no longer buffered) |
| 3 | `OUTPUT` | s→c | seq u64, bytes |
| 4 | `INPUT` | c→s | bytes |
| 5 | `RESIZE` | c→s | cols u16, rows u16 |
| 6 | `ACK` | c→s | seq u64 (lets the server trim its buffer) |
| 7 | `PING` / 8 `PONG` | both | 8 opaque bytes |
| 9 | `EXIT` | s→c | exit code i32 (the shell ended; the session is gone; 128+n for a signal) |
| 10 | `STATUS` | local only | empty to ask; the daemon answers flags u8 (bit0: a client is attached) and closes. Used by `server list`, never sent over ssh |

`HELLO` without the 0 byte (a 0.0.x client) is still valid: `TERM` is then
empty and the daemon uses `xterm-256color`.

`ACK` carries the newest seq the client has printed, sent about every
64 KiB of output.

## Resume

The daemon numbers output chunks and keeps the newest ones in a ring buffer
(4 MiB by default). On reconnect the client sends `HELLO` with the last seq
it printed:

- If everything after that seq is still buffered, it is replayed, then live
  output continues. Nothing is lost or duplicated.
- If not (the buffer wrapped while you were away), `WELCOME` sets the gap
  flag. The client clears the screen (`CSI 2J`, `CSI H`), and the daemon
  nudges the program to redraw (resizing to rows-1, then back) so
  full-screen apps repaint.
- A client whose last seq is beyond anything the daemon has numbered (it
  printed output from an earlier daemon of the same name) is treated as a
  gap and gets the whole buffer.

If the socket file disappears (for example systemd removing
`/run/user/UID` after your last login ends), the daemon recreates it within
5 s.

## Reconnect

The client watches for EOF, write errors and a PING timeout (default 15 s).
It then re-runs ssh with backoff (0.5 s, doubling, capped at 10 s), forever
until you press `Ctrl-^ .`. The status line "hqsh: reconnecting…" is written
to the terminal's title (OSC 2, after saving the title with `CSI 22;0t`,
restored with `CSI 23;0t` on reconnect), never into the screen. Keys typed
while disconnected are dropped. The first connection is different: if it
fails, hqsh exits with ssh's error instead of retrying.

Keys: `Ctrl-^ .` detaches (the session keeps running), `Ctrl-^ Ctrl-^`
sends one `Ctrl-^`, `Ctrl-^` followed by anything else sends both bytes.

The terminal goes into raw mode only after the first `WELCOME`, so ssh can
still ask for a host key confirmation or a password before that.

## Not in v1

- Local echo prediction (mosh's typing-while-lagging trick).
- UDP roaming. hqsh rides on ssh, so a network change means a reconnect,
  which is the resume above.
- A phone client. The protocol is transport-agnostic, so a WebSocket bridge
  for a PWA can come next.
