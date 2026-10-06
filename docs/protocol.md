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
  its socket). Any number of attaches can be live at once (see
  [Shared attach](#shared-attach)). A lock file next to the socket keeps it
  to one daemon per session.
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
  is an array of `{"name": "main", "attached": true, "clients": 2}` (`[]`
  when none). `attached` is true when at least one client is; `clients`
  counts them (0.2.0+; a 0.1.x daemon reports 1 or 0). The text form says
  `attached (N clients)` when more than one is. Sockets nobody answers on
  are removed.

## Frames

Every frame is `type (1 byte) | length (4 bytes, big-endian) | payload`.
Payloads are capped at 1 MiB.

| Type | Name | Direction | Payload |
|---|---|---|---|
| 1 | `HELLO` | c→s | version u16, last seq u64, cols u16, rows u16, session name, then optionally a 0 byte and the client's `TERM`, then optionally a 0 byte and flags u8 (bit0: steal, bit1: read-only) |
| 2 | `WELCOME` | s→c | version u16, first seq the server will send u64, flags u8 (bit0: gap, the client missed output that is no longer buffered; bit1: shared, the daemon does shared attach, 0.2.0+) |
| 3 | `OUTPUT` | s→c | seq u64, bytes |
| 4 | `INPUT` | c→s | bytes |
| 5 | `RESIZE` | c→s | cols u16, rows u16 |
| 6 | `ACK` | c→s | seq u64 (lets the server trim its buffer) |
| 7 | `PING` / 8 `PONG` | both | 8 opaque bytes |
| 9 | `EXIT` | s→c | exit code i32 (the shell ended; the session is gone; 128+n for a signal) |
| 10 | `STATUS` | local only | empty to ask; the daemon answers flags u8 (bit0: a client is attached), then (0.2.0+) the number of attached clients u16, and closes. Used by `server list`, never sent over ssh |
| 11 | `DETACHED` | s→c | empty: another client attached with `--steal`; stop and do not reconnect (0.2.0+) |

`HELLO` without the 0 byte (a 0.0.x client) is still valid: `TERM` is then
empty and the daemon uses `xterm-256color`. The flags byte comes after a
second 0 byte, so `TERM` stays a plain string; with no flags set (the
default, shared attach) the client leaves it out and the `HELLO` is byte for
byte what a 0.1.x client sends. When flags are set and `TERM` is empty the
payload ends `session 0 0 flags`.

`ACK` carries the newest seq the client has printed, sent about every
64 KiB of output.

## Shared attach

From 0.2.0 a session takes any number of clients at once, like tmux:

- **Output:** every attached client gets all of it, each through its own
  writer with its own position in the shared output buffer.
- **Input:** any client's `INPUT` goes to the PTY, except a client that sent
  the read-only flag (`hqsh host --read-only`): the daemon ignores its
  `INPUT` (and the client does not send keys anyway). It still gets output,
  and its `Ctrl-^ .` still detaches it.
- **Steal:** a `HELLO` with the steal flag (`hqsh host --steal`, like
  `tmux attach -d`) sends `DETACHED` to every other client and drops them.
  They exit with "detached by another client" instead of reconnecting. The
  client sets the flag on its first connection only; a reconnect never
  steals.
- **Size:** the PTY is the smallest cols and the smallest rows among the
  attached clients (read-only ones included), taken separately, as tmux
  does by default. It is recomputed on every attach, detach and `RESIZE`,
  and the PTY is resized only when the result changes. A larger client
  simply sees the session in its top-left corner.
- **Resume:** each client resumes on its own: `HELLO`'s last seq picks
  where its replay starts, exactly as with one client.
- **ACK:** the buffer is trimmed only up to the lowest seq that every
  attached client has acknowledged, so nothing a lagging client still needs
  is dropped early. The byte budget (4 MiB) still caps the buffer; a client
  that falls further behind than that gets the gap path when it comes back.
- **Backpressure:** the daemon reads the PTY ahead of the *most* caught-up
  client by at most half the buffer (2 MiB), then waits. With one client
  that is plain backpressure (a slow link slows the program, nothing is
  lost), as in 0.1. With several, a slow or stalled client never holds the
  others back: it falls behind, and once its next output has left the
  buffer, or it cannot take a frame within 5 s, the daemon drops its
  connection. It reconnects and resumes, through the gap path if needed.
- **Silence:** nobody is told when another client attaches or leaves.

### Mixing versions

- A 0.1.x client against a 0.2.0 daemon is a shared, read-write client
  (it sends no flags). It does not understand `DETACHED`, so after a steal
  it reconnects and rejoins.
- A 0.2.0 client against a 0.1.x daemon (a session started before the
  upgrade) gets the 0.1 behaviour: each attach takes over from the previous
  one, and the two reconnect over each other. `--read-only` is still kept
  by the client (it sends no keys), but a 0.1.x daemon reads the flags byte
  as part of `TERM`, so a shell it starts for that `HELLO` gets
  `xterm-256color`. The `WELCOME` shared bit tells a client which daemon it
  has. To get shared attach on an existing session, end it (`exit`) and
  attach again; the new daemon is the installed binary.

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

- Per-client windows or a status bar showing who is attached (tmux shows
  the attached sessions; hqsh stays silent).

- Local echo prediction (mosh's typing-while-lagging trick).
- UDP roaming. hqsh rides on ssh, so a network change means a reconnect,
  which is the resume above.
- A phone client. The protocol is transport-agnostic, so a WebSocket bridge
  for a PWA can come next.
