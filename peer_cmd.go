package main

import (
	"context"
	"flag"
	"fmt"
	iofs "io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/format"
	"github.com/ahaley/markroom/internal/peer"
)

func cmdPeer(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: markroom peer <add|remove|list>")
	}
	switch args[0] {
	case "add":
		return cmdPeerAdd(ctx, args[1:])
	case "remove", "rm":
		return cmdPeerRemove(ctx, args[1:])
	case "list", "ls":
		return cmdPeerList(ctx)
	default:
		return fmt.Errorf("unknown peer command %q (want add, remove, or list)", args[0])
	}
}

func cmdPeerAdd(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer add", flag.ContinueOnError)
	name := fs.String("name", "", "local label for the peer (default: its hostname)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var raw string
	if fs.NArg() > 0 {
		raw = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if raw == "" || fs.NArg() > 0 {
		return fmt.Errorf("usage: markroom peer add <url> [--name <name>]")
	}
	url, err := peer.ValidatePeerURL(raw)
	if err != nil {
		return err
	}

	cfg, err := config.EnsureIdentity()
	if err != nil {
		return err
	}
	peerName := *name
	if peerName == "" {
		peerName = defaultPeerName(url)
	}
	if err := peer.ValidRemoteName(peerName); err != nil {
		return err
	}
	for _, p := range cfg.Peers {
		if strings.EqualFold(p.Name, peerName) {
			return fmt.Errorf("a peer named %q already exists (%s)", p.Name, p.URL)
		}
		if strings.EqualFold(p.URL, url) {
			return fmt.Errorf("%s is already registered as %q", url, p.Name)
		}
	}

	// Probe before committing anything, so a typo or a host-guard refusal
	// fails here rather than silently every five minutes from now on.
	m, _, err := peer.NewClient().Manifest(ctx, url, []string{cfg.ServerID}, "")
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", url, err)
	}
	if m.Server.ID == cfg.ServerID {
		return fmt.Errorf("%s is this machine", url)
	}
	fmt.Printf("reached %q (%d root(s) advertised)\n", m.Server.Name, len(m.Roots))

	cfg.Peers = append(cfg.Peers, config.Peer{Name: peerName, URL: url, ID: m.Server.ID})
	if err := cfg.Save(); err != nil {
		return err
	}
	return runSync(ctx, cfg, peerName)
}

func cmdPeerRemove(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("peer remove", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the mirrored roots and their cached files")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var name string
	if fs.NArg() > 0 {
		name = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return err
		}
	}
	if name == "" || fs.NArg() > 0 {
		return fmt.Errorf("usage: markroom peer remove <name> [--purge]")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	kept := cfg.Peers[:0]
	found := false
	for _, p := range cfg.Peers {
		if strings.EqualFold(p.Name, name) {
			found, name = true, p.Name
			continue
		}
		kept = append(kept, p)
	}
	if !found {
		return fmt.Errorf("no peer named %q", name)
	}
	cfg.Peers = kept

	mirrors := mirrorsVia(cfg, name)
	if *purge {
		keptRoots := cfg.Roots[:0]
		for _, r := range cfg.Roots {
			if r.IsMirror() && r.ViaPeer == name {
				continue
			}
			keptRoots = append(keptRoots, r)
		}
		cfg.Roots = keptRoots
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.DeletePeerState(ctx, name); err != nil {
		return err
	}

	if !*purge {
		fmt.Printf("removed peer %q — its %d mirrored root(s) stay readable\n", name, len(mirrors))
		if len(mirrors) > 0 {
			fmt.Println("to drop them too: markroom peer remove", name, "--purge")
		}
		return nil
	}
	for _, r := range mirrors {
		if err := store.PurgeRoot(ctx, r.Name); err != nil {
			return err
		}
		if err := removeCached(r.Path); err != nil {
			return err
		}
	}
	fmt.Printf("removed peer %q and purged %d mirrored root(s)\n", name, len(mirrors))
	return nil
}

func cmdPeerList(ctx context.Context) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Peers) == 0 {
		fmt.Println("no peers configured — use: markroom peer add <url>")
		return nil
	}
	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()
	states, err := store.PeerStates(ctx)
	if err != nil {
		return err
	}

	for _, p := range cfg.Peers {
		st := states[p.Name]
		status := "never synced"
		switch {
		case st.LastError != "":
			status = "unreachable: " + st.LastError
		case !st.LastOK.IsZero():
			status = "synced " + format.TimeAgo(st.LastOK)
		}
		fmt.Printf("%-16s %s\n  %s\n", p.Name, p.URL, status)
		for _, r := range mirrorsVia(cfg, p.Name) {
			total, unread, err := store.RootStats(ctx, r.Name)
			if err != nil {
				return err
			}
			fmt.Printf("  %-14s %d docs, %d unread, %s on disk\n",
				r.Name, total, unread, humanBytes(dirSize(r.Path)))
		}
	}
	return nil
}

func cmdSync(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	only := fs.String("peer", "", "sync just this peer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	cfg, err := config.EnsureIdentity()
	if err != nil {
		return err
	}
	if len(cfg.Peers) == 0 {
		return fmt.Errorf("no peers configured — use: markroom peer add <url>")
	}
	return runSync(ctx, cfg, *only)
}

// runSync performs one sync pass and reindexes whatever it brought down.
func runSync(ctx context.Context, cfg *config.Config, only string) error {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return err
	}
	release, err := peer.Lock(cacheDir)
	if err != nil {
		return err
	}
	defer release()

	store, err := openStore()
	if err != nil {
		return err
	}
	defer store.Close()

	s := &peer.Syncer{Store: store, Client: peer.NewClient(), CacheDir: cacheDir, SelfID: cfg.ServerID}
	var res peer.Result
	if only != "" {
		cfg, res = s.SyncPeer(ctx, cfg, only)
	} else {
		cfg, res = s.SyncAll(ctx, cfg)
	}
	// Mirrored files are just files: the ordinary scanner is what turns them
	// into documents in the inbox.
	store.ScanAll(ctx, cfg.Roots)
	fmt.Println("sync:", res.Summary())
	for _, name := range sortedKeys(res.Errors) {
		fmt.Fprintf(os.Stderr, "markroom: peer %s: %v\n", name, res.Errors[name])
	}
	return nil
}

// removeCached deletes a mirrored root's files. It refuses to touch anything
// outside the cache directory, so a hand-edited config.json can never turn a
// peer removal into a recursive delete of somebody's documents.
func removeCached(path string) error {
	cacheDir, err := config.CacheDir()
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(cacheDir, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refusing to delete %s: it is not inside %s", path, cacheDir)
	}
	return os.RemoveAll(path)
}

func mirrorsVia(cfg *config.Config, peerName string) []config.Root {
	var out []config.Root
	for _, r := range cfg.Roots {
		if r.IsMirror() && r.ViaPeer == peerName {
			out = append(out, r)
		}
	}
	return out
}

// defaultPeerName derives a label from the first label of the peer's host,
// which is the machine name in both MagicDNS and ordinary LAN naming.
func defaultPeerName(url string) string {
	host := url
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	host, _, _ = strings.Cut(host, "/")
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	first, _, _ := strings.Cut(host, ".")
	return config.Slugify(first)
}

func dirSize(path string) int64 {
	var total int64
	filepath.WalkDir(path, func(_ string, d iofs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

func sortedKeys(m map[string]error) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
