package peer

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahaley/markroom/internal/config"
)

// Two unrelated machines can both call themselves "laptop", so the name a
// mirror is given has to be uniquified — and then kept, because it is
// persisted in the config and appears in URLs.
func TestEnsureMirrorRootNaming(t *testing.T) {
	cache := t.TempDir()
	s := &Syncer{CacheDir: cache, SelfID: "id-self"}
	cfg := &config.Config{ServerID: "id-self", Roots: []config.Root{{Name: "docs", Path: "/local/docs"}}}

	first := RootManifest{
		Name:   "notes",
		Origin: OriginRef{ID: "id-one", Name: "laptop", Root: "notes"},
		Hops:   []string{"id-one"},
	}
	r1, isNew, err := s.ensureMirrorRoot(cfg, first, "one")
	if err != nil || !isNew || r1 == nil {
		t.Fatalf("first mirror: %v %v %v", r1, isNew, err)
	}
	if r1.Name != "laptop:notes" {
		t.Errorf("name = %q, want laptop:notes", r1.Name)
	}
	if want := filepath.Join(cache, "laptop", "notes"); r1.Path != want {
		t.Errorf("path = %q, want %q", r1.Path, want)
	}

	// A different machine, same self-chosen name.
	second := RootManifest{
		Name:   "notes",
		Origin: OriginRef{ID: "id-two", Name: "laptop", Root: "notes"},
		Hops:   []string{"id-two"},
	}
	r2, isNew, err := s.ensureMirrorRoot(cfg, second, "two")
	if err != nil || !isNew || r2 == nil {
		t.Fatalf("second mirror: %v %v %v", r2, isNew, err)
	}
	if r2.Name != "laptop-2:notes" {
		t.Errorf("name = %q, want laptop-2:notes", r2.Name)
	}
	if r2.Path == r1.Path {
		t.Errorf("both mirrors landed in %q", r1.Path)
	}

	// Seeing the first origin again resolves to the same root, not a third.
	again, isNew, err := s.ensureMirrorRoot(cfg, first, "one")
	if err != nil || isNew {
		t.Fatalf("re-seeing an origin created a new root: %v %v", isNew, err)
	}
	if again.Name != "laptop:notes" {
		t.Errorf("name drifted to %q", again.Name)
	}
	if n := len(cfg.Roots); n != 3 {
		t.Errorf("roots = %d, want 3 (local + two mirrors)", n)
	}
}

// The origin's own root name survives verbatim, including a space; only the
// server half is slugified, because that half becomes a directory.
func TestMirrorNameKeepsRootVerbatim(t *testing.T) {
	cache := t.TempDir()
	s := &Syncer{CacheDir: cache, SelfID: "id-self"}
	cfg := &config.Config{ServerID: "id-self"}
	rm := RootManifest{
		Name:   "api docs",
		Origin: OriginRef{ID: "id-one", Name: "My Laptop", Root: "api docs"},
		Hops:   []string{"id-one"},
	}
	r, _, err := s.ensureMirrorRoot(cfg, rm, "one")
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "my-laptop:api docs" {
		t.Errorf("name = %q", r.Name)
	}
	// The colon in the root name must not survive into the cache path (a
	// Windows drive letter has one of its own, so only check below the cache
	// directory).
	under, err := filepath.Rel(cache, r.Path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(under, ":") || strings.Contains(under, " ") {
		t.Errorf("cache path below the cache dir = %q, want slugified", under)
	}
}

// When two peers offer the same origin root, the shorter path wins and then
// ownership stays put, so the source can't flap between them.
func TestMirrorSourceElection(t *testing.T) {
	s := &Syncer{CacheDir: t.TempDir(), SelfID: "id-self"}
	cfg := &config.Config{ServerID: "id-self"}

	viaLong := RootManifest{
		Origin: OriginRef{ID: "id-one", Name: "laptop", Root: "notes"},
		Hops:   []string{"id-one", "id-mid"},
	}
	viaShort := RootManifest{
		Origin: OriginRef{ID: "id-one", Name: "laptop", Root: "notes"},
		Hops:   []string{"id-one"},
	}

	r, _, _ := s.ensureMirrorRoot(cfg, viaLong, "faraway")
	if r == nil || r.ViaPeer != "faraway" {
		t.Fatalf("first claim = %+v", r)
	}
	// A direct route is strictly better, so it takes over.
	r, _, _ = s.ensureMirrorRoot(cfg, viaShort, "direct")
	if r == nil || r.ViaPeer != "direct" {
		t.Fatalf("shorter path did not win: %+v", r)
	}
	// The longer one must not take it back.
	if r, _, _ := s.ensureMirrorRoot(cfg, viaLong, "faraway"); r != nil {
		t.Errorf("longer path reclaimed the root: %+v", r)
	}
	if n := len(cfg.Roots); n != 1 {
		t.Errorf("roots = %d, want 1", n)
	}
}

func TestValidateRootManifest(t *testing.T) {
	good := RootManifest{
		Name:   "notes",
		Origin: OriginRef{ID: "id-one", Name: "laptop", Root: "notes"},
		Hops:   []string{"id-one"},
	}
	if err := validateRootManifest(good); err != nil {
		t.Errorf("valid manifest rejected: %v", err)
	}

	noOrigin := good
	noOrigin.Origin.ID = ""
	if err := validateRootManifest(noOrigin); err == nil {
		t.Error("accepted a root with no origin id")
	}

	badName := good
	badName.Origin.Name = "../evil"
	if err := validateRootManifest(badName); err == nil {
		t.Error("accepted an origin name that would escape the cache directory")
	}

	badRoot := good
	badRoot.Origin.Root = "a/b"
	if err := validateRootManifest(badRoot); err == nil {
		t.Error("accepted an origin root name containing a separator")
	}

	tooFar := good
	for i := 0; i <= MaxHops; i++ {
		tooFar.Hops = append(tooFar.Hops, "id")
	}
	if err := validateRootManifest(tooFar); err == nil {
		t.Error("accepted a root beyond the hop limit")
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err != ErrLocked {
		t.Errorf("second Lock = %v, want ErrLocked", err)
	}
	release()
	release2, err := Lock(dir)
	if err != nil {
		t.Fatalf("Lock after release = %v", err)
	}
	release2()
}
