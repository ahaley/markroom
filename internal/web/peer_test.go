package web

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
	"github.com/ahaley/markroom/internal/peer"
)

// machine is one markroom host in a peering test: its own store, config,
// cache directory and (once serve is called) its own URL. Chaining tests
// stand several of these up and point them at each other.
type machine struct {
	t     *testing.T
	srv   *Server
	store *index.Store
	cfg   *config.Config
	cache string
	url   string
}

func newMachine(t *testing.T, name string) *machine {
	t.Helper()
	store, err := index.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	m := &machine{
		t:     t,
		store: store,
		cache: t.TempDir(),
		cfg:   &config.Config{ServerID: "id-" + name, ServerName: name},
	}
	m.srv = NewServer(store, m.cfg)
	// Read m.cfg at call time so a test that swaps in an updated config (as
	// a sync does) is picked up by the handlers.
	m.srv.reloadConfig = func() (*config.Config, error) { return m.cfg, nil }
	return m
}

// addRoot registers a new local root and returns its directory.
func (m *machine) addRoot(name string) string {
	m.t.Helper()
	dir := m.t.TempDir()
	m.cfg.Roots = append(m.cfg.Roots, config.Root{Name: name, Path: dir})
	m.srv.cfg.Store(m.cfg)
	return dir
}

// serve starts an HTTP server for this machine and returns its base URL.
func (m *machine) serve() string {
	m.t.Helper()
	ts := httptest.NewServer(m.srv.Handler())
	m.t.Cleanup(ts.Close)
	m.url = ts.URL
	return ts.URL
}

func (m *machine) rescan() {
	m.t.Helper()
	m.srv.reloadAndScan(m.t.Context())
}

// peerWith registers another machine as a peer of this one.
func (m *machine) peerWith(name string, other *machine) {
	m.t.Helper()
	if other.url == "" {
		m.t.Fatalf("peer %q is not serving yet", name)
	}
	m.cfg.Peers = append(m.cfg.Peers, config.Peer{Name: name, URL: other.url})
	m.srv.cfg.Store(m.cfg)
}

// syncer builds a Syncer wired to this machine's in-memory config rather than
// to the real one on disk.
func (m *machine) syncer() *peer.Syncer {
	return &peer.Syncer{
		Store:      m.store,
		Client:     peer.NewClient(),
		CacheDir:   m.cache,
		SelfID:     m.cfg.ServerID,
		LoadConfig: func() (*config.Config, error) { return m.cfg, nil },
		SaveConfig: func(c *config.Config) error { m.cfg = c; return nil },
	}
}

// sync performs one full sync pass and then indexes what it brought down,
// exactly as `markroom sync` and the serve daemon both do.
func (m *machine) sync() peer.Result {
	m.t.Helper()
	cfg, res := m.syncer().SyncAll(m.t.Context(), m.cfg)
	m.cfg = cfg
	m.srv.cfg.Store(cfg)
	m.store.ScanAll(m.t.Context(), cfg.Roots)
	return res
}

// mirror returns the local root mirroring the given origin root, or nil.
func (m *machine) mirror(name string) *config.Root {
	return m.cfg.Find(name)
}

// fetchManifest gets the manifest from url, asserting a 200.
func fetchManifest(t *testing.T, url string) peer.Manifest {
	t.Helper()
	resp, err := http.Get(url + peer.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("manifest = %d, want 200", resp.StatusCode)
	}
	var m peer.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func findRoot(m peer.Manifest, name string) *peer.RootManifest {
	for i := range m.Roots {
		if m.Roots[i].Name == name {
			return &m.Roots[i]
		}
	}
	return nil
}

func TestManifestShape(t *testing.T) {
	a := newMachine(t, "alpha")
	dir := a.addRoot("notes")
	writeFile(t, filepath.Join(dir, "spec.md"), "# The Spec\n\nhello body text")
	a.rescan()
	url := a.serve()

	m := fetchManifest(t, url)
	if m.Protocol != peer.Protocol {
		t.Errorf("protocol = %d, want %d", m.Protocol, peer.Protocol)
	}
	if m.Server.ID != "id-alpha" || m.Server.Name != "alpha" {
		t.Errorf("server = %+v", m.Server)
	}
	rm := findRoot(m, "notes")
	if rm == nil {
		t.Fatalf("root %q missing from %+v", "notes", m.Roots)
	}
	// A local root originates here and has travelled exactly one hop: us.
	if rm.Origin.ID != "id-alpha" || rm.Origin.Root != "notes" {
		t.Errorf("origin = %+v", rm.Origin)
	}
	if len(rm.Hops) != 1 || rm.Hops[0] != "id-alpha" {
		t.Errorf("hops = %v, want [id-alpha]", rm.Hops)
	}
	if len(rm.Docs) != 1 {
		t.Fatalf("docs = %+v", rm.Docs)
	}
	d := rm.Docs[0]
	if d.Path != "spec.md" || d.Title != "The Spec" || d.Size == 0 || d.MTime == 0 {
		t.Errorf("doc = %+v", d)
	}
	// The advertised hash must be the one the index holds, or an incremental
	// sync would refetch everything forever.
	stored, err := a.store.GetDoc(t.Context(), "notes", "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if d.Hash != stored.Hash {
		t.Errorf("advertised hash %q != indexed hash %q", d.Hash, stored.Hash)
	}
}

// A re-exported mirror keeps the original origin and grows its hop path.
func TestManifestReExport(t *testing.T) {
	b := newMachine(t, "beta")
	dir := b.t.TempDir()
	b.cfg.Roots = append(b.cfg.Roots, config.Root{
		Name: "alpha:notes", Path: dir,
		OriginID: "id-alpha", OriginName: "alpha", OriginRoot: "notes",
		Hops: []string{"id-alpha"}, ViaPeer: "alpha",
	})
	b.srv.cfg.Store(b.cfg)
	writeFile(t, filepath.Join(dir, "spec.md"), "# The Spec")
	b.rescan()

	rm := findRoot(fetchManifest(t, b.serve()), "alpha:notes")
	if rm == nil {
		t.Fatal("mirrored root not advertised")
	}
	if rm.Origin.ID != "id-alpha" || rm.Origin.Name != "alpha" || rm.Origin.Root != "notes" {
		t.Errorf("origin should still be alpha's, got %+v", rm.Origin)
	}
	want := []string{"id-alpha", "id-beta"}
	if len(rm.Hops) != 2 || rm.Hops[0] != want[0] || rm.Hops[1] != want[1] {
		t.Errorf("hops = %v, want %v", rm.Hops, want)
	}
}

// A caller announcing itself must not be handed back its own documents.
func TestManifestSeen(t *testing.T) {
	b := newMachine(t, "beta")
	local := b.addRoot("work")
	writeFile(t, filepath.Join(local, "own.md"), "# Own")
	mirrored := b.t.TempDir()
	b.cfg.Roots = append(b.cfg.Roots, config.Root{
		Name: "alpha:notes", Path: mirrored,
		OriginID: "id-alpha", OriginName: "alpha", OriginRoot: "notes",
		Hops: []string{"id-alpha"}, ViaPeer: "alpha",
	})
	b.srv.cfg.Store(b.cfg)
	writeFile(t, filepath.Join(mirrored, "spec.md"), "# The Spec")
	b.rescan()
	url := b.serve()

	resp, err := http.Get(url + peer.ManifestPath + "?seen=id-alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m peer.Manifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if findRoot(m, "alpha:notes") != nil {
		t.Error("beta re-exported alpha's own root back to alpha")
	}
	if findRoot(m, "work") == nil {
		t.Error("beta withheld its own root from alpha")
	}

	// Talking to yourself is an error, not an empty manifest.
	resp2, err := http.Get(url + peer.ManifestPath + "?seen=other,id-beta")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("self-seen manifest = %d, want 409", resp2.StatusCode)
	}
}

func TestManifestETag(t *testing.T) {
	a := newMachine(t, "alpha")
	dir := a.addRoot("notes")
	writeFile(t, filepath.Join(dir, "spec.md"), "# The Spec")
	a.rescan()
	url := a.serve() + peer.ManifestPath

	first, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	first.Body.Close()
	etag := first.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag on manifest")
	}

	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("If-None-Match", etag)
	second, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("conditional manifest = %d, want 304", second.StatusCode)
	}
}

func TestManifestNoIdentity(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	get(t, ts.Client(), ts.URL+peer.ManifestPath, http.StatusServiceUnavailable, "no peer identity")
}

// Mirroring must not touch the origin's own inbox.
func TestRawDoesNotMarkRead(t *testing.T) {
	a := newMachine(t, "alpha")
	dir := a.addRoot("notes")
	writeFile(t, filepath.Join(dir, "spec.md"), "# The Spec\n\nbody")
	a.rescan()
	url := a.serve()

	body := get(t, http.DefaultClient, url+peer.RawPath+"notes/spec.md", http.StatusOK, "# The Spec")
	if body != "# The Spec\n\nbody" {
		t.Errorf("raw body = %q, want the file verbatim", body)
	}
	doc, err := a.store.GetDoc(t.Context(), "notes", "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "new" {
		t.Errorf("status after a peer fetch = %q, want new", doc.Status)
	}
}

// The headline case: alpha's markdown becomes readable on beta, and then on
// gamma, still attributed to alpha and without the name growing per hop.
func TestPeerChainTwoHops(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "spec.md"), "# The Spec\n\nwritten on alpha")
	writeFile(t, filepath.Join(aDir, "img", "flow.png"), "not-really-a-png")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	bDir := beta.addRoot("work")
	writeFile(t, filepath.Join(bDir, "own.md"), "# Beta's Own")
	beta.rescan()
	beta.serve()
	beta.peerWith("alpha", alpha)

	if res := beta.sync(); len(res.Errors) > 0 {
		t.Fatalf("sync errors: %v", res.Errors)
	}

	// Beta now holds alpha's root under an origin-qualified name, with the
	// bytes actually on its own disk.
	mirror := beta.mirror("alpha:notes")
	if mirror == nil {
		t.Fatalf("beta has no alpha:notes; roots = %+v", beta.cfg.Roots)
	}
	if !mirror.IsMirror() || mirror.OriginName != "alpha" || mirror.ViaPeer != "alpha" {
		t.Errorf("mirror metadata = %+v", *mirror)
	}
	cached := filepath.Join(mirror.Path, "spec.md")
	if body, err := os.ReadFile(cached); err != nil {
		t.Fatalf("reading cached doc: %v", err)
	} else if string(body) != "# The Spec\n\nwritten on alpha" {
		t.Errorf("cached body = %q", body)
	}
	// Attachments come along, or every mirrored doc with an image breaks.
	if _, err := os.Stat(filepath.Join(mirror.Path, "img", "flow.png")); err != nil {
		t.Errorf("asset not mirrored: %v", err)
	}

	// And beta serves it like anything else.
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/spec.md", http.StatusOK, "written on alpha")
	get(t, http.DefaultClient, beta.url+"/", http.StatusOK, "The Spec", "Beta&#39;s Own")

	// Gamma, one hop further out, gets alpha's root through beta.
	gamma := newMachine(t, "gamma")
	gamma.serve()
	gamma.peerWith("beta", beta)
	if res := gamma.sync(); len(res.Errors) > 0 {
		t.Fatalf("gamma sync errors: %v", res.Errors)
	}

	// The name does not accumulate hops, and the origin is still alpha.
	if gamma.mirror("beta:alpha:notes") != nil {
		t.Error("root name grew with the chain")
	}
	g := gamma.mirror("alpha:notes")
	if g == nil {
		t.Fatalf("gamma has no alpha:notes; roots = %+v", gamma.cfg.Roots)
	}
	if g.OriginName != "alpha" || g.OriginID != "id-alpha" {
		t.Errorf("gamma lost the original origin: %+v", *g)
	}
	if g.ViaPeer != "beta" {
		t.Errorf("gamma fetched via %q, want beta", g.ViaPeer)
	}
	if len(g.Hops) != 2 || g.Hops[0] != "id-alpha" || g.Hops[1] != "id-beta" {
		t.Errorf("hops = %v, want [id-alpha id-beta]", g.Hops)
	}
	get(t, http.DefaultClient, gamma.url+"/d/alpha:notes/spec.md", http.StatusOK, "written on alpha")
	// Beta's own work came along too.
	if gamma.mirror("beta:work") == nil {
		t.Error("gamma did not mirror beta's own root")
	}
}

// The sync button: a running server pulls from its peers on demand, without
// the operator dropping to a shell.
func TestSyncEndpoint(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "spec.md"), "# The Spec\n\nwritten on alpha")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	beta.serve()

	// With no peers the button isn't offered, and the endpoint is a no-op.
	body := get(t, http.DefaultClient, beta.url+"/", http.StatusOK)
	if strings.Contains(body, "/api/sync") {
		t.Error("sync button shown with no peers configured")
	}

	beta.peerWith("alpha", alpha)
	beta.srv.syncer = beta.syncer()
	get(t, http.DefaultClient, beta.url+"/", http.StatusOK, `action="/api/sync"`)

	resp, err := http.Post(beta.url+"/api/sync", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK { // the client follows the redirect home
		t.Fatalf("POST /api/sync = %d", resp.StatusCode)
	}
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/spec.md", http.StatusOK, "written on alpha")
}

// A reader has to be able to tell whose machine a document came from.
func TestOriginShownInUI(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "spec.md"), "# The Spec\n\nwritten on alpha")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	bDir := beta.addRoot("work")
	writeFile(t, filepath.Join(bDir, "own.md"), "# Beta's Own")
	beta.rescan()
	beta.serve()
	beta.peerWith("alpha", alpha)
	beta.sync()

	// Inbox: the mirrored row and its pill are marked, the local ones aren't.
	body := get(t, http.DefaultClient, beta.url+"/", http.StatusOK,
		`title="mirrored from alpha"`, "↗ alpha", "pill mirror")
	if n := strings.Count(body, "↗ alpha"); n != 1 {
		t.Errorf("origin badge appeared %d times, want 1 (only the mirrored row)", n)
	}

	// Document view says so too.
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/spec.md", http.StatusOK,
		`class="origin"`, "↗ alpha")

	// A local document carries no origin at all.
	local := get(t, http.DefaultClient, beta.url+"/d/work/own.md", http.StatusOK)
	if strings.Contains(local, `class="origin"`) {
		t.Error("a local document was labelled as mirrored")
	}
}

// When a peer stops answering, the inbox should say so rather than quietly
// serving stale documents as if nothing happened.
func TestUnreachablePeerShownInInbox(t *testing.T) {
	alpha := newMachine(t, "alpha")
	writeFile(t, filepath.Join(alpha.addRoot("notes"), "spec.md"), "# The Spec")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	beta.serve()
	beta.peerWith("alpha", alpha)
	beta.sync()

	body := get(t, http.DefaultClient, beta.url+"/", http.StatusOK)
	if strings.Contains(body, "not answering") {
		t.Error("a healthy peer was reported as unreachable")
	}

	beta.cfg.Peers[0].URL = deadURL(t)
	beta.sync()
	get(t, http.DefaultClient, beta.url+"/", http.StatusOK,
		"peerstatus", "alpha is not answering", "showing what was mirrored")
}

// The reason the cache exists: the laptop closes and the documents stay.
func TestPeerOfflineKeepsCache(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "spec.md"), "# The Spec\n\nwritten on alpha")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	beta.serve()
	beta.peerWith("alpha", alpha)
	beta.sync()
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/spec.md", http.StatusOK, "written on alpha")

	// Alpha goes away. Point beta at a port nothing answers on.
	beta.cfg.Peers[0].URL = deadURL(t)
	res := beta.sync()
	if len(res.Errors) == 0 {
		t.Error("syncing an unreachable peer reported no error")
	}
	if res.Pruned != 0 {
		t.Errorf("pruned %d files from an unreachable peer's cache", res.Pruned)
	}

	// The whole point: still readable.
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/spec.md", http.StatusOK, "written on alpha")
	get(t, http.DefaultClient, beta.url+"/", http.StatusOK, "The Spec")
}

// Two machines pointed at each other must not trade the same documents back
// and forth forever.
func TestPeerCycleRejected(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "spec.md"), "# The Spec")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	bDir := beta.addRoot("work")
	writeFile(t, filepath.Join(bDir, "own.md"), "# Beta's Own")
	beta.rescan()
	beta.serve()

	alpha.peerWith("beta", beta)
	beta.peerWith("alpha", alpha)

	beta.sync()  // beta picks up alpha:notes
	alpha.sync() // alpha must not pick its own documents back up
	beta.sync()  // and the second round must not grow either

	for _, r := range alpha.cfg.Roots {
		if r.IsMirror() && r.OriginID == "id-alpha" {
			t.Errorf("alpha re-imported its own root as %q", r.Name)
		}
	}
	if alpha.mirror("beta:work") == nil {
		t.Error("loop detection also blocked a legitimate root")
	}
	if n := len(beta.cfg.Roots); n != 2 {
		t.Errorf("beta has %d roots, want 2 (own + alpha's): %+v", n, beta.cfg.Roots)
	}
}

// A document deleted upstream should disappear here — but only when we were
// actually able to read the manifest that says so.
func TestPeerPruneOnDelete(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	writeFile(t, filepath.Join(aDir, "keep.md"), "# Keep")
	writeFile(t, filepath.Join(aDir, "sub", "gone.md"), "# Gone")
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	beta.serve()
	beta.peerWith("alpha", alpha)
	beta.sync()

	mirror := beta.mirror("alpha:notes")
	if mirror == nil {
		t.Fatal("no mirror")
	}
	get(t, http.DefaultClient, beta.url+"/d/alpha:notes/sub/gone.md", http.StatusOK, "Gone")

	if err := os.Remove(filepath.Join(aDir, "sub", "gone.md")); err != nil {
		t.Fatal(err)
	}
	alpha.rescan()
	if res := beta.sync(); res.Pruned != 1 {
		t.Errorf("pruned %d, want 1", res.Pruned)
	}
	if _, err := os.Stat(filepath.Join(mirror.Path, "sub", "gone.md")); !os.IsNotExist(err) {
		t.Error("deleted doc still cached")
	}
	if _, err := os.Stat(filepath.Join(mirror.Path, "sub")); !os.IsNotExist(err) {
		t.Error("emptied directory not pruned")
	}
	if _, err := os.Stat(filepath.Join(mirror.Path, "keep.md")); err != nil {
		t.Errorf("pruning took a live document: %v", err)
	}
	if _, err := beta.store.GetDoc(t.Context(), "alpha:notes", "sub/gone.md"); err == nil {
		t.Error("pruned doc still in the index")
	}
}

// A mirrored file keeps the origin's timestamp, so the inbox shows when the
// document was written rather than when it was copied — and so the scanner's
// (mtime, size) shortcut still recognizes it as unchanged.
func TestMirrorMTimePreserved(t *testing.T) {
	alpha := newMachine(t, "alpha")
	aDir := alpha.addRoot("notes")
	src := filepath.Join(aDir, "spec.md")
	writeFile(t, src, "# The Spec")
	want := time.Now().Add(-72 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(src, want, want); err != nil {
		t.Fatal(err)
	}
	alpha.rescan()
	alpha.serve()

	beta := newMachine(t, "beta")
	beta.serve()
	beta.peerWith("alpha", alpha)
	if res := beta.sync(); res.Fetched != 1 {
		t.Fatalf("first sync fetched %d, want 1", res.Fetched)
	}

	st, err := os.Stat(filepath.Join(beta.mirror("alpha:notes").Path, "spec.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.ModTime().Equal(want) {
		t.Errorf("mirrored mtime = %v, want %v", st.ModTime(), want)
	}

	// Nothing changed upstream, so a second pass must move no bytes.
	if res := beta.sync(); res.Fetched != 0 {
		t.Errorf("second sync fetched %d, want 0", res.Fetched)
	}
}

// deadURL returns a loopback URL on a port nothing is listening on, which is
// how a test stands in for a machine that has gone to sleep.
func deadURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func TestRawRejects(t *testing.T) {
	a := newMachine(t, "alpha")
	dir := a.addRoot("notes")
	writeFile(t, filepath.Join(dir, "tool.exe"), "MZ")
	writeFile(t, filepath.Join(dir, "img.png"), "not-really-a-png")
	a.rescan()
	url := a.serve() + peer.RawPath

	get(t, http.DefaultClient, url+"notes/img.png", http.StatusOK)
	get(t, http.DefaultClient, url+"notes/tool.exe", http.StatusNotFound)
	get(t, http.DefaultClient, url+"notes/..%2F..%2Fsecret.md", http.StatusNotFound)
	get(t, http.DefaultClient, url+"nope/spec.md", http.StatusNotFound)
}
