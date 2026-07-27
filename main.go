// markroom — a reading-first daemon for the markdown your agents write.
//
//	markroom add <dir> [--name <name>]   register a directory of markdown
//	markroom remove <name>               unregister a directory (and purge its index)
//	markroom list                        show registered directories
//	markroom serve [--addr host:port]    run the reading server
//	markroom tui [--root <name>]         read in the terminal
//	markroom peer add <url>              mirror another markroom's markdown
//	markroom sync                        pull from every peer now
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
	"github.com/ahaley/markroom/internal/tui"
	"github.com/ahaley/markroom/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch os.Args[1] {
	case "add":
		err = cmdAdd(ctx, os.Args[2:])
	case "remove":
		err = cmdRemove(ctx, os.Args[2:])
	case "list":
		err = cmdList(ctx)
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "tui":
		err = cmdTUI(ctx, os.Args[2:])
	case "peer":
		err = cmdPeer(ctx, os.Args[2:])
	case "sync":
		err = cmdSync(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "markroom: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "markroom:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Println(`markroom — read the markdown your agents write

usage:
  markroom add <dir> [--name <name>]   register a directory of markdown
  markroom remove <name>               unregister a directory
  markroom list                        show registered directories
  markroom serve [--addr host:port]    run the reading server (default 127.0.0.1:8383)
                 [--allow-host h1,h2]  extra hostnames accepted by the server
                                       (localhost, the bound host, loopback IPs,
                                       and *.ts.net are always accepted)
                 [--sync-every 5m]     how often to pull from peers (0 disables)
  markroom tui [--root <name>]         read in the terminal (optionally filtered
                                       to one root)

peering — read another machine's markdown here, and keep reading it after
that machine goes offline:
  markroom peer add <url> [--name n]   mirror another markroom's markdown
  markroom peer remove <name>          stop syncing (mirrored docs stay)
                 [--purge]             …and delete what was mirrored
  markroom peer list                   peers, last sync, and what they gave us
  markroom sync [--peer <name>]        pull from peers now

To read from your phone over Tailscale:
  tailscale serve --bg http://127.0.0.1:8383`)
}

// openStore opens the index database in the user's config directory.
func openStore() (*index.Store, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return index.Open(filepath.Join(dir, "index.db"))
}

func cmdAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "name for the root (default: directory name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var dir string
	if fs.NArg() > 0 {
		dir = fs.Arg(0)
		// Re-parse the remainder so `markroom add <dir> --name x` works too.
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if dir == "" || fs.NArg() > 0 {
		return fmt.Errorf("usage: markroom add <dir> [--name <name>]")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	rootName := *name
	if rootName == "" {
		rootName = config.Slugify(filepath.Base(abs))
	}
	if err := config.ValidRootName(rootName); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	for _, r := range cfg.Roots {
		if strings.EqualFold(r.Name, rootName) {
			return fmt.Errorf("a root named %q already exists (%s)", r.Name, r.Path)
		}
		if strings.EqualFold(filepath.Clean(r.Path), filepath.Clean(abs)) {
			return fmt.Errorf("%s is already registered as %q", abs, r.Name)
		}
	}
	cfg.Roots = append(cfg.Roots, config.Root{Name: rootName, Path: abs})
	if err := cfg.Save(); err != nil {
		return err
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	n, err := store.ScanRoot(ctx, config.Root{Name: rootName, Path: abs})
	if err != nil {
		return err
	}
	fmt.Printf("added %q (%s) — indexed %d document(s)\n", rootName, abs, n)
	return nil
}

func cmdRemove(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: markroom remove <name>")
	}
	name := args[0]
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	kept := cfg.Roots[:0]
	found := false
	var removed config.Root
	for _, r := range cfg.Roots {
		if strings.EqualFold(r.Name, name) {
			// While the peer is still configured, removing its mirror here
			// would only last until the next sync recreated it — the peer is
			// what has to go. Once the peer is gone the mirror is an orphan,
			// and removing it is exactly right.
			if r.IsMirror() && cfg.FindPeer(r.ViaPeer) != nil {
				return fmt.Errorf("%s is mirrored from peer %q — use: markroom peer remove %s [--purge]", r.Name, r.ViaPeer, r.ViaPeer)
			}
			found, name, removed = true, r.Name, r
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("no root named %q", name)
	}
	cfg.Roots = kept
	if err := cfg.Save(); err != nil {
		return err
	}
	// An orphaned mirror's files are markroom's own cache, so they go too;
	// a local root's directory is the user's and is never touched.
	if removed.IsMirror() {
		if err := removeCached(removed.Path); err != nil {
			return err
		}
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.PurgeRoot(ctx, name); err != nil {
		return err
	}
	fmt.Printf("removed %q\n", name)
	return nil
}

func cmdList(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		fmt.Println("no roots registered — use: markroom add <dir>")
		return nil
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	for _, r := range cfg.Roots {
		total, unread, err := store.RootStats(ctx, r.Name)
		if err != nil {
			return err
		}
		fmt.Printf("%-20s %s  (%d docs, %d unread)\n", r.Name, r.Path, total, unread)
	}
	return nil
}

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8383", "listen address")
	allow := fs.String("allow-host", "", "comma-separated extra hostnames to accept")
	syncEvery := fs.Duration("sync-every", 5*time.Minute, "how often to pull from peers (0 disables)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var allowHosts []string
	for _, h := range strings.Split(*allow, ",") {
		if h = strings.TrimSpace(h); h != "" {
			allowHosts = append(allowHosts, h)
		}
	}

	// Serving means being peerable, which requires a stable identity.
	cfg, err := config.EnsureIdentity()
	if err != nil {
		return err
	}
	cacheDir, err := config.CacheDir()
	if err != nil {
		return err
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	return web.NewServer(store, cfg).Run(ctx, web.Options{
		Addr:       *addr,
		AllowHosts: allowHosts,
		CacheDir:   cacheDir,
		SyncEvery:  *syncEvery,
	})
}

func cmdTUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	root := fs.String("root", "", "start filtered to this root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *root != "" {
		found := false
		for _, r := range cfg.Roots {
			if strings.EqualFold(r.Name, *root) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("no root named %q", *root)
		}
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	return tui.Run(ctx, store, cfg, *root)
}
