// markroom — a reading-first daemon for the markdown your agents write.
//
//	markroom add <dir> [--name <name>]   register a directory of markdown
//	markroom remove <name>               unregister a directory (and purge its index)
//	markroom list                        show registered directories
//	markroom serve [--addr host:port]    run the reading server
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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

To read from your phone over Tailscale:
  tailscale serve --bg http://127.0.0.1:8383`)
}

func cmdAdd(ctx context.Context, args []string) error {
	var dir, name string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			name = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("unknown flag %q", args[i])
		case dir == "":
			dir = args[i]
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}
	if dir == "" {
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
	if name == "" {
		name = slugify(filepath.Base(abs))
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	for _, r := range cfg.Roots {
		if strings.EqualFold(r.Name, name) {
			return fmt.Errorf("a root named %q already exists (%s)", r.Name, r.Path)
		}
		if strings.EqualFold(filepath.Clean(r.Path), filepath.Clean(abs)) {
			return fmt.Errorf("%s is already registered as %q", abs, r.Name)
		}
	}
	cfg.Roots = append(cfg.Roots, Root{Name: name, Path: abs})
	if err := cfg.save(); err != nil {
		return err
	}

	store, err := openDefaultStore()
	if err != nil {
		return err
	}
	defer store.Close()
	n, err := store.ScanRoot(ctx, Root{Name: name, Path: abs})
	if err != nil {
		return err
	}
	fmt.Printf("added %q (%s) — indexed %d document(s)\n", name, abs, n)
	return nil
}

func cmdRemove(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: markroom remove <name>")
	}
	name := args[0]
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	kept := cfg.Roots[:0]
	found := false
	for _, r := range cfg.Roots {
		if strings.EqualFold(r.Name, name) {
			found = true
			name = r.Name
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return fmt.Errorf("no root named %q", name)
	}
	cfg.Roots = kept
	if err := cfg.save(); err != nil {
		return err
	}
	store, err := openDefaultStore()
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
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	if len(cfg.Roots) == 0 {
		fmt.Println("no roots registered — use: markroom add <dir>")
		return nil
	}
	store, err := openDefaultStore()
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

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "root"
	}
	return out
}
