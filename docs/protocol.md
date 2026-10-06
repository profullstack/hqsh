# hqsh protocol (v1, draft)

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
  starting the daemon first if needed. A newer attach takes over from an
  older one.

## Frames

Every frame is `type (1 byte) | length (4 bytes, big-endian) | payload`.
Payloads are capped at 1 MiB.

| Type | Name | Direction | Payload |
|---|---|---|---|
| 1 | `HELLO` | c→s | version u16, last seq u64, cols u16, rows u16, session name |
| 2 | `WELCOME` | s→c | version u16, first seq the server will send u64, flags u8 (bit0: gap, the client missed output that is no longer buffered) |
| 3 | `OUTPUT` | s→c | seq u64, bytes |
| 4 | `INPUT` | c→s | bytes |
| 5 | `RESIZE` | c→s | cols u16, rows u16 |
| 6 | `ACK` | c→s | seq u64 (lets the server trim its buffer) |
| 7 | `PING` / 8 `PONG` | both | 8 opaque bytes |
| 9 | `EXIT` | s→c | exit code i32 (the shell ended; the session is gone) |

## Resume

The daemon numbers output chunks and keeps the newest ones in a ring buffer
(4 MiB by default). On reconnect the client sends `HELLO` with the last seq
it printed:

- If everything after that seq is still buffered, it is replayed, then live
  output continues. Nothing is lost or duplicated.
- If not (the buffer wrapped while you were away), `WELCOME` sets the gap
  flag. The client clears the screen, and the daemon nudges the program to
  redraw (SIGWINCH via a resize) so full-screen apps repaint.

## Reconnect

The client watches for EOF, write errors and a PING timeout (default 15 s).
It then re-runs ssh with backoff (0.5 s, doubling, capped at 10 s), forever
until you press `Ctrl-^ .`. The status line "hqsh: reconnecting…" is written
to the terminal's title, never into the screen.

## Not in v1

- Local echo prediction (mosh's typing-while-lagging trick).
- UDP roaming. hqsh rides on ssh, so a network change means a reconnect,
  which is the resume above.
- A phone client. The protocol is transport-agnostic, so a WebSocket bridge
  for a PWA can come next.
