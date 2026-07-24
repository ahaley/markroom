# markroom

The room where your agents' markdown develops. `markroom` is a reading-first
daemon: whitelist directories from the command line, and it indexes every
markdown file in them, tracks what you've read, and serves a clean long-form
reading UI you can reach from your phone over Tailscale.

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
markroom tui [--root <name>]         read in the terminal
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
| `u` | (in a document) mark unread |
| `esc` / `q` | back / quit |

Start filtered to one root with `markroom tui --root myapp`.

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
contents you trust.

## Where state lives

Config (`config.json`) and the index (`index.db`) are stored in the platform's
standard config location:

| Platform | Path |
|----------|------|
| Windows  | `%AppData%\markroom\` |
| macOS    | `~/Library/Application Support/markroom/` |
| Linux    | `$XDG_CONFIG_HOME/markroom/` (default `~/.config/markroom/`) |

Deleting that directory resets `markroom` completely; your markdown files are
never touched.

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
