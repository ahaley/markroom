# markroom

The room where your agents' markdown develops. `markroom` is a reading-first
daemon: whitelist directories from the command line, and it indexes every
markdown file in them, tracks what you've read, and serves a clean long-form
reading UI you can reach from your phone over Tailscale. Point several machines
at each other and every inbox shows all of their markdown — mirrored locally,
so it stays readable after a laptop closes for the day.

Runs on Windows, macOS, and Linux — it's a single static Go binary with no
cgo and no runtime dependencies.

## Install

```sh
# macOS / Linux
go build -o markroom .

# Windows
go build -o markroom.exe .
```

Put the binary somewhere on your PATH (e.g. `/usr/local/bin`, `~/bin`, or a
directory already on `%PATH%` on Windows).

Cross-compiling for another machine works the usual Go way, e.g.
`GOOS=linux GOARCH=amd64 go build -o markroom .`

## Usage

```
markroom add <dir> [--name <name>]   register a directory of markdown
markroom remove <name>               unregister a directory (purges its index)
markroom list                        show registered directories with unread counts
markroom serve [--addr host:port]    run the reading server (default 127.0.0.1:8383)
               [--allow-host h1,h2]  extra hostnames accepted by the server
               [--sync-every 5m]     how often to pull from peers (0 disables)
markroom tui [--root <name>]         read in the terminal

markroom peer add <url> [--name n]   mirror another markroom's markdown
markroom peer remove <name>          stop syncing (mirrored docs stay readable)
                     [--purge]       …and delete what was mirrored
markroom peer list                   peers, last sync, and what they gave us
markroom sync [--peer <name>]        pull from peers now
```

Register the places your agents write to, then leave the server running:

```sh
# macOS / Linux
markroom add ~/projects/myapp/docs --name myapp

# Windows
markroom add C:\Projects\myapp\docs --name myapp

markroom serve
```

Open http://127.0.0.1:8383 — the inbox lists every document, unread first.
Opening a document marks it read; when an agent regenerates a file you've
already read, it comes back flagged **updated**.

## Reading in the terminal

`markroom tui` opens the same inbox → document flow in your terminal — no
server needed (though it happily runs alongside one). Documents render with
styled headings, tables, and highlighted code.

| Key | Action |
|-----|--------|
| `enter` | open the selected document (marks it read) |
| `/` | search titles and bodies (`esc` clears) |
| `tab` / `shift+tab` | cycle the root filter |
| `r` | rescan all roots now |
| `s` | pull from peers now (shown only when peers are configured) |
| `u` | (in a document) mark unread |
| `esc` / `q` | back / quit |

Start filtered to one root with `markroom tui --root myapp`.

## Peering: many machines, one inbox

Your agents write markdown on more than one machine. Point each `markroom` at
the others and every inbox shows all of it.

```sh
# on the laptop
markroom add ~/projects/notes --name notes
markroom serve --addr 0.0.0.0:8383

# on the desktop
markroom peer add http://laptop.tailnet.ts.net:8383
markroom list        # notes now appears as "laptop:notes"
markroom serve
```

The desktop's inbox now holds both its own documents and the laptop's, each row
badged **↗ laptop** so you can tell whose machine it came from.

**The documents are copied, not proxied.** Peering mirrors every markdown file
(and the images they reference) into a local cache, so when the laptop lid
closes for the day its documents stay readable on the desktop. An unreachable
peer is never fatal: the inbox says it isn't answering and keeps serving what
it already has.

**Peering chains.** Anything a machine mirrors it also re-serves, so a third
machine peering with the desktop sees the laptop's documents too — still
attributed to `laptop`, not to the desktop it arrived through. Names don't
grow as the chain lengthens, and a root never travels back to the machine it
came from, so mutual peers can't loop.

| | |
|---|---|
| Root naming | A peer's root `notes` becomes `laptop:notes` locally, so two machines can both have a root called `notes` |
| Read state | Local to each machine — reading something on the desktop doesn't mark it read on the laptop |
| Sync timing | Every 5 minutes while serving (`--sync-every`), the ⇅ button, `s` in the TUI, or `markroom sync` |
| Removing a peer | `markroom peer remove <name>` stops syncing but keeps what was mirrored; add `--purge` to delete it |

Both machines have to be reachable from each other — a tailnet is the intended
arrangement. Serving on `0.0.0.0` rather than the default loopback is what makes
a machine peerable; see the security note below.

## Reading from your phone

The server binds localhost by default. Expose it on your tailnet with:

```sh
tailscale serve --bg http://127.0.0.1:8383
```

The same command works on every platform (on Windows, run it from a shell
where `tailscale.exe` is on PATH).

## Security model

`markroom` binds to localhost and serves your files without authentication —
the network boundary (loopback, or your tailnet via `tailscale serve`) is the
access control. Requests whose `Host` header isn't `localhost`, a loopback IP,
the address you bound, or a `*.ts.net` name are rejected with 403. This blocks
DNS rebinding, where a malicious website points its own domain at `127.0.0.1`
to read your documents through your browser. If you front `markroom` with your
own reverse proxy or hostname, allow it explicitly:

```sh
markroom serve --allow-host docs.example.internal
```

Markdown is rendered with raw HTML enabled, so only register directories whose
contents you trust. Pages that render document content send a Content Security
Policy that blocks scripts — there is no JavaScript in the UI, so nothing
legitimate is lost, and it limits what a hostile document can do.

**Peering widens this.** The peer endpoints (`/api/peer/…`) are unauthenticated,
exactly like the reading UI, so anyone who can reach a peerable `markroom` can
read everything it holds and mirror it. A peer's markdown is also somebody
else's content rendered on your machine. Peer only with machines you trust, on
a network you trust — a tailnet with ACLs, not the open internet. If you bind a
peerable server to `0.0.0.0`, the `Host` guard still applies: reaching it by
raw Tailscale IP needs `--allow-host 100.x.y.z`, while MagicDNS names work out
of the box.

## Where state lives

Config (`config.json`), the index (`index.db`), and mirrored peer content
(`cache/`) are stored in the platform's standard config location:

| Platform | Path |
|----------|------|
| Windows  | `%AppData%\markroom\` |
| macOS    | `~/Library/Application Support/markroom/` |
| Linux    | `$XDG_CONFIG_HOME/markroom/` (default `~/.config/markroom/`) |

Deleting that directory resets `markroom` completely; your markdown files are
never touched. `markroom peer list` reports how much disk each peer's cache is
using.

## How it works

- The daemon rescans all roots every 30 seconds (plus the ↻ button for an
  immediate rescan). Scans skip dot-directories, `node_modules`, `vendor`,
  `bin`, `obj`, `dist`, `build`, `target`, `__pycache__`.
- Full-text search is SQLite FTS5 across titles and bodies (pure-Go SQLite,
  so cross-compilation stays trivial).
- Read state is keyed to a content hash, so a regenerated document
  automatically returns to the unread pile as "updated".
- Documents are served at `/d/<root>/<relative-path>`, so relative links and
  images between markdown files resolve naturally.
- Peers exchange a manifest of paths and content hashes, so a sync that finds
  nothing new transfers nothing. Mirrored files keep the origin's modification
  time, so the inbox shows when a document was written rather than when it was
  copied. Mirrored roots are ordinary directories on disk, which is why the
  scanner, the reading server, and the TUI all treat them like local ones.

## Running as a background service

- **Windows** — Task Scheduler ("At log on", run `markroom.exe serve`), or a
  service wrapper like NSSM.
- **macOS** — a `launchd` user agent in `~/Library/LaunchAgents` running
  `markroom serve`.
- **Linux** — a systemd user unit, e.g.
  `~/.config/systemd/user/markroom.service` with
  `ExecStart=%h/bin/markroom serve`, then
  `systemctl --user enable --now markroom`.

## The name

Like a darkroom, but for markdown: the quiet room where what your agents
develop comes into view.
