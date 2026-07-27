package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"My Docs", "my-docs"},
		{"CBM.Gestalt", "cbm-gestalt"},
		{"already-fine_slug", "already-fine_slug"},
		{"--trim--", "trim"},
		{"日本語", "root"},
		{"", "root"},
	}
	for _, tt := range tests {
		if got := Slugify(tt.in); got != tt.want {
			t.Errorf("Slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidRootName(t *testing.T) {
	ok := []string{"docs", "api docs", "API-Docs", "a_b.c", "日本語"}
	for _, s := range ok {
		if err := ValidRootName(s); err != nil {
			t.Errorf("ValidRootName(%q) = %v, want nil", s, err)
		}
	}
	bad := []string{"", ".", "..", "laptop:notes", "a/b", `a\b`, "a\x00b", string(make([]byte, 65))}
	for _, s := range bad {
		if err := ValidRootName(s); err == nil {
			t.Errorf("ValidRootName(%q) = nil, want error", s)
		}
	}
}

func TestMirrorRootName(t *testing.T) {
	// The origin's own root name is preserved verbatim; only the server name
	// is slugified, since that half becomes a cache directory component.
	if got := MirrorRootName("My Laptop", "api docs"); got != "my-laptop:api docs" {
		t.Errorf("MirrorRootName = %q", got)
	}
}

// A config written before peering existed must load unchanged.
func TestLoadBackCompat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	old := `{"roots":[{"name":"docs","path":"/tmp/docs"}]}`
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0].Name != "docs" {
		t.Fatalf("roots = %+v", cfg.Roots)
	}
	if cfg.ServerID != "" || len(cfg.Peers) != 0 {
		t.Errorf("expected zero identity and peers, got %q %+v", cfg.ServerID, cfg.Peers)
	}
	if cfg.Roots[0].IsMirror() {
		t.Error("a legacy root should not read as a mirror")
	}
}

func TestSaveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	want := &Config{
		ServerID:   "abc123",
		ServerName: "desktop",
		Roots: []Root{
			{Name: "docs", Path: "/tmp/docs"},
			{Name: "laptop:notes", Path: "/tmp/cache/laptop/notes",
				OriginID: "def456", OriginName: "laptop", OriginRoot: "notes",
				Hops: []string{"def456"}, ViaPeer: "laptop"},
		},
		Peers: []Peer{{Name: "laptop", URL: "http://laptop:8383", ID: "def456"}},
	}
	if err := want.saveTo(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Error("temp file survived a successful save")
	}
	got, err := loadFrom(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerID != want.ServerID || got.ServerName != want.ServerName {
		t.Errorf("identity = %q/%q", got.ServerID, got.ServerName)
	}
	m := got.Find("laptop:notes")
	if m == nil {
		t.Fatal("mirror root missing after round trip")
	}
	if !m.IsMirror() || m.OriginName != "laptop" || m.OriginRoot != "notes" ||
		len(m.Hops) != 1 || m.Hops[0] != "def456" || m.ViaPeer != "laptop" {
		t.Errorf("mirror metadata lost: %+v", *m)
	}
	if got.Find("docs").IsMirror() {
		t.Error("local root came back as a mirror")
	}
	if p := got.FindPeer("laptop"); p == nil || p.ID != "def456" {
		t.Errorf("peer = %+v", p)
	}
	if got.FindPeer("nope") != nil {
		t.Error("FindPeer found a peer that isn't there")
	}
}
