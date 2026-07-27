// Package config stores the whitelist of markdown roots, this machine's peer
// identity, and the peers it mirrors from, in the platform's standard user
// config directory.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Root is a directory of markdown files this machine serves. A root is either
// local (registered with `markroom add`) or a mirror of a root belonging to
// another server, cached under CacheDir by the syncer.
type Root struct {
	Name string `json:"name"`
	Path string `json:"path"`

	// Mirror metadata. All empty on local roots. Origin* describe where the
	// documents were *originally* written, which survives re-export down a
	// chain of servers; ViaPeer is merely who we currently pull from.
	OriginID   string   `json:"origin_id,omitempty"`
	OriginName string   `json:"origin_name,omitempty"` // display only; may change
	OriginRoot string   `json:"origin_root,omitempty"` // origin's own name for it
	Hops       []string `json:"hops,omitempty"`        // server IDs, origin first
	ViaPeer    string   `json:"via_peer,omitempty"`
}

// IsMirror reports whether the root is a cached copy of another server's root.
func (r Root) IsMirror() bool { return r.OriginID != "" }

// Peer is another markroom server whose roots we mirror.
type Peer struct {
	Name string `json:"name"`         // local label, e.g. "laptop"
	URL  string `json:"url"`          // e.g. "http://laptop.tailnet.ts.net:8383"
	ID   string `json:"id,omitempty"` // learned from the peer's first manifest
}

type Config struct {
	// ServerID is a stable random identifier for this machine. It is what
	// loop detection compares, because ServerName is user-chosen and two
	// machines may well pick the same one.
	ServerID   string `json:"server_id,omitempty"`
	ServerName string `json:"server_name,omitempty"`
	Roots      []Root `json:"roots"`
	Peers      []Peer `json:"peers,omitempty"`
}

// Dir returns (creating if necessary) the markroom config directory.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "markroom")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// CacheDir returns (creating if necessary) the directory holding mirrored
// copies of peers' markdown.
func CacheDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return "", err
	}
	return cache, nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func Load() (*Config, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	return loadFrom(p)
}

func loadFrom(p string) (*Config, error) {
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", p, err)
	}
	return &cfg, nil
}

func (c *Config) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	return c.saveTo(p)
}

// saveTo writes via a temp file and a rename, so a reader never observes a
// half-written config and a crash mid-write can't truncate the real one.
func (c *Config) saveTo(p string) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// EnsureIdentity loads the config and, if this machine has no peer identity
// yet, mints one and saves it. The ID must be stable across runs: a fresh one
// each time would silently defeat loop detection, which is why Load never
// invents it.
func EnsureIdentity() (*Config, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	changed := false
	if cfg.ServerID == "" {
		var b [10]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, err
		}
		cfg.ServerID = hex.EncodeToString(b[:])
		changed = true
	}
	if cfg.ServerName == "" {
		host, _ := os.Hostname()
		cfg.ServerName = Slugify(host)
		if cfg.ServerName == "root" { // Slugify's fallback; not a useful machine name
			cfg.ServerName = "markroom"
		}
		changed = true
	}
	if changed {
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// Find returns the root with the given name, or nil.
func (c *Config) Find(name string) *Root {
	for i := range c.Roots {
		if c.Roots[i].Name == name {
			return &c.Roots[i]
		}
	}
	return nil
}

// FindPeer returns the peer with the given name, or nil.
func (c *Config) FindPeer(name string) *Peer {
	for i := range c.Peers {
		if c.Peers[i].Name == name {
			return &c.Peers[i]
		}
	}
	return nil
}

// Slugify reduces a string to a lowercase identifier safe as a filesystem
// path component and a URL segment. It returns "root" if nothing survives.
func Slugify(s string) string {
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

// ValidRootName checks a name usable as a root: it becomes one URL path
// segment, so it may not contain a separator, and ":" is reserved for the
// mirror namespace (see MirrorRootName). It is deliberately permissive
// otherwise — names like "api docs" predate this check and still work.
func ValidRootName(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("name is empty")
	case len(s) > 64:
		return fmt.Errorf("name %q is longer than 64 characters", s)
	case s == "." || s == "..":
		return fmt.Errorf("name %q is reserved", s)
	case strings.Contains(s, ":"):
		return fmt.Errorf("name %q contains ':', which is reserved for mirrored roots", s)
	case strings.ContainsAny(s, `/\`):
		return fmt.Errorf("name %q contains a path separator", s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name %q contains a control character", s)
		}
	}
	return nil
}

// MirrorRootName is the local name for a root mirrored from another server:
// the origin's slugified name, a colon, then the origin's own root name.
// Namespacing by *origin* rather than by the peer we fetched from is what
// keeps names from growing as a chain lengthens — machine 3 pulling machine
// 1's "notes" through machine 2 still calls it "m1:notes".
func MirrorRootName(originName, originRoot string) string {
	return Slugify(originName) + ":" + originRoot
}
