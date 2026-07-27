package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
	"github.com/ahaley/markroom/internal/peer"
)

// handleManifest advertises what this server holds, so another markroom can
// mirror it. Everything here is derived from the index and the config — no
// disk walk — so it stays cheap enough to poll.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg.Load()
	if cfg.ServerID == "" {
		http.Error(w, "markroom: this server has no peer identity yet", http.StatusServiceUnavailable)
		return
	}

	// A caller announces the server IDs it already stands for. If we are in
	// that set we are talking to ourselves through some loop in the mesh.
	seen := parseSeen(r.URL.Query().Get("seen"))
	if seen[cfg.ServerID] {
		http.Error(w, "markroom: that is this server", http.StatusConflict)
		return
	}

	byRoot, err := s.store.AllDocMeta(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m := peer.Manifest{
		Protocol:    peer.Protocol,
		Server:      peer.ServerRef{ID: cfg.ServerID, Name: cfg.ServerName},
		GeneratedAt: time.Now().Unix(),
		Caps:        []string{"raw"},
		Roots:       []peer.RootManifest{},
	}
	for _, rt := range cfg.Roots {
		rm, ok := manifestRoot(cfg, rt, byRoot[rt.Name])
		if !ok || peer.HopsContain(rm.Hops, seen) {
			continue
		}
		m.Roots = append(m.Roots, rm)
	}

	body, err := json.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The ETag covers GeneratedAt too, so it only ever saves the client the
	// body — but a manifest is the large half of a no-op sync, and a peer
	// polls it far more often than its documents change.
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(body)
}

// manifestRoot describes one root as a peer should see it, reporting false if
// the root should not be advertised at all.
func manifestRoot(cfg *config.Config, rt config.Root, docs []index.DocMeta) (peer.RootManifest, bool) {
	origin := peer.OriginRef{ID: cfg.ServerID, Name: cfg.ServerName, Root: rt.Name}
	// Advertised hops are always "where it has been" plus us, which for a
	// local root is just us.
	hops := append(append([]string{}, rt.Hops...), cfg.ServerID)
	if rt.IsMirror() {
		origin = peer.OriginRef{ID: rt.OriginID, Name: rt.OriginName, Root: rt.OriginRoot}
	}
	if len(hops) > peer.MaxHops {
		return peer.RootManifest{}, false
	}

	rm := peer.RootManifest{
		Name:   rt.Name,
		Origin: origin,
		Hops:   hops,
		Docs:   make([]peer.DocEntry, 0, len(docs)),
	}
	for _, d := range docs {
		rm.Docs = append(rm.Docs, peer.DocEntry{
			Path:  d.RelPath,
			Hash:  d.Hash,
			MTime: d.MTime,
			Size:  d.Size,
			Title: d.Title,
			Words: d.Words,
		})
	}
	rm.Assets = scanAssets(rt.Path)
	return rm, true
}

// Bounds on the asset listing. Unlike documents, assets aren't in the index,
// so this is a real disk walk on every manifest build — it stays cheap by
// hiding the same directories the scanner hides and refusing to advertise
// anything a reader wouldn't want pulled over the network anyway.
const (
	maxAssetsPerRoot = 20_000
	maxAssetBytes    = 32 << 20
)

// scanAssets lists the non-markdown files a peer needs so that the images and
// attachments inside mirrored documents still resolve.
func scanAssets(rootPath string) []peer.AssetEntry {
	var out []peer.AssetEntry
	err := filepath.WalkDir(rootPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != rootPath && index.SkipDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(out) >= maxAssetsPerRoot {
			return filepath.SkipAll
		}
		if !assetExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxAssetBytes {
			return nil
		}
		rel, err := filepath.Rel(rootPath, path)
		if err != nil {
			return nil
		}
		out = append(out, peer.AssetEntry{
			Path:  filepath.ToSlash(rel),
			MTime: info.ModTime().Unix(),
			Size:  info.Size(),
		})
		return nil
	})
	if err != nil {
		slog.Warn("listing assets", "root", rootPath, "err", err)
	}
	return out
}

// handleRaw serves a file's bytes to a mirroring peer. Unlike the reading
// view it deliberately does not upsert or mark anything read: a downstream
// sync must never disturb this machine's own inbox.
func (s *Server) handleRaw(w http.ResponseWriter, r *http.Request) {
	_, _, full, err := resolveFile(s.cfg.Load(), r.PathValue("root"), r.PathValue("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !index.IsMarkdown(full) && !assetExts[strings.ToLower(filepath.Ext(full))] {
		http.NotFound(w, r)
		return
	}
	// Never let a peer's markdown be interpreted as a document by a browser
	// that wandered onto this endpoint.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, full)
}

func parseSeen(q string) map[string]bool {
	seen := map[string]bool{}
	for _, id := range strings.Split(q, ",") {
		if id = strings.TrimSpace(id); id != "" {
			seen[id] = true
		}
	}
	return seen
}
