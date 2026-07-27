package peer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
)

// Syncer mirrors peers' markdown into a local cache directory. Each mirrored
// root becomes an ordinary config.Root pointing into that cache, which is
// what lets the rest of markroom — the scanner, the reading server, the TUI —
// treat a peer's documents exactly like local ones, including after the peer
// goes offline.
type Syncer struct {
	Store    *index.Store
	Client   *Client
	CacheDir string
	SelfID   string

	// LoadConfig and SaveConfig reach the config on disk. They are fields so
	// tests can stand up whole machines without touching the real one.
	LoadConfig func() (*config.Config, error)
	SaveConfig func(*config.Config) error
}

func (s *Syncer) load() (*config.Config, error) {
	if s.LoadConfig != nil {
		return s.LoadConfig()
	}
	return config.Load()
}

func (s *Syncer) save(c *config.Config) error {
	if s.SaveConfig != nil {
		return s.SaveConfig(c)
	}
	return c.Save()
}

// Result summarizes one pass over every peer. Peer failures are collected
// rather than returned: one unreachable laptop must not stop the rest, and
// must not disturb what is already cached.
type Result struct {
	Fetched  int
	Pruned   int
	NewRoots int
	Errors   map[string]error
}

func (r Result) failed() []string {
	names := make([]string, 0, len(r.Errors))
	for n := range r.Errors {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Summary renders the result as one line for a CLI or a log.
func (r Result) Summary() string {
	s := fmt.Sprintf("fetched %d, pruned %d", r.Fetched, r.Pruned)
	if r.NewRoots > 0 {
		s += fmt.Sprintf(", %d new root(s)", r.NewRoots)
	}
	if names := r.failed(); len(names) > 0 {
		s += " — unreachable: " + strings.Join(names, ", ")
	}
	return s
}

// SyncAll syncs every configured peer and returns the config to use from
// here on: mirrored roots discovered during the pass have been merged into
// it and saved. cfg is not modified.
func (s *Syncer) SyncAll(ctx context.Context, cfg *config.Config) (*config.Config, Result) {
	res := Result{Errors: map[string]error{}}
	work := cloneConfig(cfg)
	touched := map[string]bool{}

	for _, p := range work.Peers {
		if err := s.syncPeer(ctx, work, p, touched, &res); err != nil {
			res.Errors[p.Name] = err
		}
	}
	if len(touched) == 0 && !peersChanged(cfg, work) {
		return cfg, res
	}

	return s.commit(work, touched), res
}

// SyncPeer syncs a single named peer.
func (s *Syncer) SyncPeer(ctx context.Context, cfg *config.Config, name string) (*config.Config, Result) {
	res := Result{Errors: map[string]error{}}
	p := cfg.FindPeer(name)
	if p == nil {
		res.Errors[name] = fmt.Errorf("no peer named %q", name)
		return cfg, res
	}
	work := cloneConfig(cfg)
	touched := map[string]bool{}
	if err := s.syncPeer(ctx, work, *p, touched, &res); err != nil {
		res.Errors[name] = err
	}
	if len(touched) == 0 && !peersChanged(cfg, work) {
		return cfg, res
	}
	return s.commit(work, touched), res
}

// commit persists what a sync discovered. It re-reads the config immediately
// before writing so that a `markroom add` which landed while we were on the
// network isn't thrown away by our own save.
func (s *Syncer) commit(work *config.Config, touched map[string]bool) *config.Config {
	fresh, err := s.load()
	if err != nil {
		slog.Warn("reloading config before save", "err", err)
		fresh = work
	} else {
		mergeInto(fresh, work, touched)
	}
	if err := s.save(fresh); err != nil {
		slog.Warn("saving config after sync", "err", err)
		return work
	}
	return fresh
}

func (s *Syncer) syncPeer(ctx context.Context, cfg *config.Config, p config.Peer, touched map[string]bool, res *Result) error {
	state := index.PeerState{Peer: p.Name, ServerID: p.ID, LastTry: time.Now()}
	if prior, err := s.Store.PeerStates(ctx); err == nil {
		if ps, ok := prior[p.Name]; ok {
			state.LastOK = ps.LastOK
			state.ManifestETag = ps.ManifestETag
		}
	}
	record := func(err error) {
		if err != nil {
			state.LastError = err.Error()
		} else {
			state.LastError = ""
			state.LastOK = time.Now()
		}
		if setErr := s.Store.SetPeerState(ctx, state); setErr != nil {
			slog.Warn("recording peer state", "peer", p.Name, "err", setErr)
		}
	}

	m, etag, err := s.Client.Manifest(ctx, p.URL, []string{s.SelfID}, state.ManifestETag)
	if errors.Is(err, ErrNotModified) {
		record(nil)
		return nil
	}
	if err != nil {
		record(err)
		return err
	}
	if m.Server.ID == s.SelfID {
		err := fmt.Errorf("peer %q is this machine", p.Name)
		record(err)
		return err
	}
	if p.ID != "" && p.ID != m.Server.ID {
		// Not fatal — a reinstall legitimately changes the ID — but the
		// operator should know the identity behind the URL moved.
		slog.Warn("peer identity changed", "peer", p.Name, "was", p.ID, "now", m.Server.ID)
	}
	state.ServerID = m.Server.ID
	state.ManifestETag = etag
	setPeerID(cfg, p.Name, m.Server.ID)

	for _, rm := range m.Roots {
		if err := s.syncRoot(ctx, cfg, p, rm, touched, res); err != nil {
			slog.Warn("mirroring root", "peer", p.Name, "root", rm.Name, "err", err)
		}
	}
	record(nil)
	return nil
}

// syncRoot mirrors one advertised root: it decides on a local name and cache
// directory, fetches whatever is missing or stale, and removes whatever the
// origin no longer has.
func (s *Syncer) syncRoot(ctx context.Context, cfg *config.Config, p config.Peer, rm RootManifest, touched map[string]bool, res *Result) error {
	// Independently of the filter we asked the peer to apply, refuse anything
	// that has already passed through us. A peer running an older build — or
	// a hostile one — would otherwise hand our own documents back as if they
	// were someone else's.
	if rm.Origin.ID == s.SelfID || containsID(rm.Hops, s.SelfID) {
		return nil
	}
	if err := validateRootManifest(rm); err != nil {
		return err
	}
	root, isNew, err := s.ensureMirrorRoot(cfg, rm, p.Name)
	if err != nil || root == nil {
		return err
	}
	if isNew {
		res.NewRoots++
	}
	touched[root.Name] = true
	if err := os.MkdirAll(root.Path, 0o700); err != nil {
		return err
	}

	have, err := s.Store.DocHashes(ctx, root.Name)
	if err != nil {
		return err
	}

	keep := map[string]bool{}
	for _, d := range rm.Docs {
		rel, err := SafeRelPath(d.Path)
		if err != nil {
			slog.Warn("rejecting path from peer", "peer", p.Name, "err", err)
			continue
		}
		full, err := InsideRoot(root.Path, rel)
		if err != nil {
			slog.Warn("rejecting path from peer", "peer", p.Name, "err", err)
			continue
		}
		keep[rel] = true
		if have[rel] == d.Hash {
			continue // already mirrored at this exact content
		}
		body, err := s.Client.Fetch(ctx, p.URL, rm.Name, d.Path)
		if err != nil {
			// One bad file must not abandon the rest of the root, and must
			// not let the pruner treat it as deleted upstream.
			slog.Warn("fetching doc", "peer", p.Name, "path", d.Path, "err", err)
			continue
		}
		sum := sha256.Sum256(body)
		if got := hex.EncodeToString(sum[:]); got != d.Hash {
			slog.Warn("hash mismatch, discarding", "peer", p.Name, "path", d.Path)
			continue
		}
		if err := writeMirrored(full, body, time.Unix(d.MTime, 0)); err != nil {
			slog.Warn("writing mirrored doc", "path", full, "err", err)
			continue
		}
		res.Fetched++
	}

	for _, a := range rm.Assets {
		rel, err := SafeRelPath(a.Path)
		if err != nil {
			continue
		}
		full, err := InsideRoot(root.Path, rel)
		if err != nil {
			continue
		}
		keep[rel] = true
		// Assets carry no hash, so mtime and size are the freshness signal —
		// the same heuristic the local scanner already trusts.
		if st, err := os.Stat(full); err == nil && st.Size() == a.Size && st.ModTime().Unix() == a.MTime {
			continue
		}
		body, err := s.Client.Fetch(ctx, p.URL, rm.Name, a.Path)
		if err != nil {
			slog.Warn("fetching asset", "peer", p.Name, "path", a.Path, "err", err)
			continue
		}
		if err := writeMirrored(full, body, time.Unix(a.MTime, 0)); err != nil {
			slog.Warn("writing mirrored asset", "path", full, "err", err)
			continue
		}
		res.Fetched++
	}

	// We reached the end of a manifest we successfully read, so anything in
	// the cache the origin didn't mention is genuinely gone. (A failed
	// manifest never gets here, which is what keeps an offline peer's cache
	// intact.)
	n, err := prune(root.Path, keep)
	res.Pruned += n
	return err
}

func validateRootManifest(rm RootManifest) error {
	if rm.Origin.ID == "" {
		return errors.New("root advertised with no origin id")
	}
	if len(rm.Hops) > MaxHops {
		return fmt.Errorf("root %q has travelled %d hops, more than the %d limit", rm.Name, len(rm.Hops), MaxHops)
	}
	if err := ValidRemoteName(rm.Origin.Name); err != nil {
		return fmt.Errorf("origin name: %w", err)
	}
	if err := config.ValidRootName(rm.Origin.Root); err != nil {
		return fmt.Errorf("origin root: %w", err)
	}
	if len(rm.Docs) > MaxDocsPerRoot {
		return fmt.Errorf("root %q advertises %d docs, more than the %d limit", rm.Name, len(rm.Docs), MaxDocsPerRoot)
	}
	return nil
}

// ensureMirrorRoot returns the local root that should hold this advertised
// root, creating it if we have never seen it. It returns a nil root, and no
// error, when another peer is already a shorter path to the same origin.
func (s *Syncer) ensureMirrorRoot(cfg *config.Config, rm RootManifest, viaPeer string) (*config.Root, bool, error) {
	hops := append([]string{}, rm.Hops...)

	for i := range cfg.Roots {
		r := &cfg.Roots[i]
		if r.OriginID != rm.Origin.ID || r.OriginRoot != rm.Origin.Root {
			continue
		}
		// Two peers can offer the same origin. Prefer the shorter path, and
		// otherwise leave ownership where it is so the source can't flap.
		if r.ViaPeer != viaPeer && len(r.Hops) <= len(hops) {
			return nil, false, nil
		}
		r.OriginName = rm.Origin.Name
		r.Hops = hops
		r.ViaPeer = viaPeer
		return r, false, nil
	}

	name := uniqueRootName(cfg, rm.Origin.Name, rm.Origin.Root)
	path, err := s.cachePath(cfg, rm.Origin.Name, rm.Origin.Root)
	if err != nil {
		return nil, false, err
	}
	cfg.Roots = append(cfg.Roots, config.Root{
		Name: name, Path: path,
		OriginID: rm.Origin.ID, OriginName: rm.Origin.Name, OriginRoot: rm.Origin.Root,
		Hops: hops, ViaPeer: viaPeer,
	})
	return &cfg.Roots[len(cfg.Roots)-1], true, nil
}

// uniqueRootName suffixes the origin half until the name is free. Server
// names are user-chosen, so two unrelated machines really can both be
// "laptop"; the assigned name is then persisted and never recomputed.
func uniqueRootName(cfg *config.Config, originName, originRoot string) string {
	base := config.Slugify(originName)
	for n := 1; ; n++ {
		cand := base + ":" + originRoot
		if n > 1 {
			cand = fmt.Sprintf("%s-%d:%s", base, n, originRoot)
		}
		if cfg.Find(cand) == nil {
			return cand
		}
	}
}

// cachePath picks a directory for a new mirror. It is derived from slugs, not
// from the root name, so a colon or a space never reaches the filesystem.
func (s *Syncer) cachePath(cfg *config.Config, originName, originRoot string) (string, error) {
	if s.CacheDir == "" {
		return "", errors.New("no cache directory configured")
	}
	claimed := map[string]bool{}
	for _, r := range cfg.Roots {
		claimed[strings.ToLower(filepath.Clean(r.Path))] = true
	}
	server, root := config.Slugify(originName), config.Slugify(originRoot)
	for n := 1; ; n++ {
		dir := filepath.Join(s.CacheDir, server, root)
		if n > 1 {
			dir = filepath.Join(s.CacheDir, fmt.Sprintf("%s-%d", server, n), root)
		}
		if !claimed[strings.ToLower(filepath.Clean(dir))] {
			return dir, nil
		}
	}
}

// writeMirrored writes through a temp file so a reader never sees a partial
// document, then restores the origin's modification time. The mtime matters
// more than it looks: without it the scanner's (mtime, size) shortcut would
// treat every file as changed after every sync, and the inbox would show
// when we copied a document rather than when it was written.
func writeMirrored(full string, body []byte, mtime time.Time) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	tmp := full + partSuffix
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, full); err != nil {
		// Windows refuses to replace a file another process has open; the
		// next sync will try again.
		time.Sleep(50 * time.Millisecond)
		if err2 := os.Rename(tmp, full); err2 != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Chtimes(full, mtime, mtime)
}

const partSuffix = ".markroom-part"

// prune deletes cached files the origin no longer advertises, then removes
// the directories left empty behind them.
func prune(rootPath string, keep map[string]bool) (int, error) {
	var dead []string
	var dirs []string
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != rootPath {
				dirs = append(dirs, path)
			}
			return nil
		}
		rel, err := filepath.Rel(rootPath, path)
		if err != nil {
			return nil
		}
		if !keep[filepath.ToSlash(rel)] {
			dead = append(dead, path)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range dead {
		if err := os.Remove(p); err == nil {
			n++
		}
	}
	// Deepest first, so a directory emptied by the pass above also goes.
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		os.Remove(d) // fails harmlessly when the directory still has contents
	}
	return n, nil
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func setPeerID(cfg *config.Config, name, id string) {
	if p := cfg.FindPeer(name); p != nil {
		p.ID = id
	}
}

func cloneConfig(c *config.Config) *config.Config {
	out := *c
	out.Roots = append([]config.Root{}, c.Roots...)
	out.Peers = append([]config.Peer{}, c.Peers...)
	return &out
}

func peersChanged(a, b *config.Config) bool {
	if len(a.Peers) != len(b.Peers) {
		return true
	}
	for i := range a.Peers {
		if a.Peers[i] != b.Peers[i] {
			return true
		}
	}
	return false
}

// mergeInto applies the mirror roots this sync touched, and any peer IDs it
// learned, onto a config freshly read from disk.
func mergeInto(fresh, work *config.Config, touched map[string]bool) {
	for _, r := range work.Roots {
		if !r.IsMirror() || !touched[r.Name] {
			continue
		}
		if existing := fresh.Find(r.Name); existing != nil {
			*existing = r
		} else {
			fresh.Roots = append(fresh.Roots, r)
		}
	}
	for _, p := range work.Peers {
		if fp := fresh.FindPeer(p.Name); fp != nil {
			fp.ID = p.ID
		}
	}
}
