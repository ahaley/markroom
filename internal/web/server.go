// Package web serves the reading UI: the inbox, rendered documents, and the
// small API the pages post back to.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	highlighting "github.com/yuin/goldmark-highlighting/v2"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
)

// Extensions of non-markdown files we are willing to serve from a root,
// so relative images and attachments inside documents resolve.
var assetExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".txt": true, ".json": true, ".csv": true, ".pdf": true,
}

var md = goldmark.New(
	goldmark.WithExtensions(
		extension.GFM,
		extension.Footnote,
		highlighting.NewHighlighting(
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
	),
	goldmark.WithParserOptions(
		parser.WithAutoHeadingID(),
	),
	goldmark.WithRendererOptions(
		// Personal tool reading local files: allow raw HTML so README-style
		// badges, <details>, and agent-emitted tables render.
		html.WithUnsafe(),
	),
)

// Server serves the reading UI over a Store and the current config.
type Server struct {
	store *index.Store
	cfg   atomic.Pointer[config.Config]

	// reloadConfig refreshes the whitelist before a rescan; overridable in
	// tests so handlers never touch the real user config.
	reloadConfig func() (*config.Config, error)
}

func NewServer(store *index.Store, cfg *config.Config) *Server {
	s := &Server{store: store, reloadConfig: config.Load}
	s.cfg.Store(cfg)
	return s
}

// Run scans, keeps rescanning every 30 seconds, and serves until ctx is
// canceled, then drains in-flight requests.
func (s *Server) Run(ctx context.Context, addr string, allowHosts []string) error {
	fmt.Println("markroom: initial scan…")
	s.store.ScanAll(ctx, s.cfg.Load().Roots)

	// Periodic rescan keeps the index tracking whatever agents write.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reloadAndScan(ctx)
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           hostGuard(addr, allowHosts, s.Handler()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	fmt.Printf("markroom: oh, hi — reading at http://%s\n", addr)
	fmt.Println("markroom: for your phone over Tailscale:  tailscale serve --bg http://" + addr)

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return nil
	}
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleInbox)
	mux.HandleFunc("GET /d/{root}/{path...}", s.handleDoc)
	mux.HandleFunc("POST /api/unread", s.handleUnread)
	mux.HandleFunc("POST /api/rescan", s.handleRescan)
	mux.HandleFunc("GET /app.css", s.handleCSS)
	return mux
}

// reloadAndScan picks up config changes and rescans every root.
func (s *Server) reloadAndScan(ctx context.Context) {
	if fresh, err := s.reloadConfig(); err == nil {
		s.cfg.Store(fresh)
	}
	s.store.ScanAll(ctx, s.cfg.Load().Roots)
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	cfg := s.cfg.Load()

	// An unknown root is almost always a stale bookmark for a removed root;
	// degrade to the full inbox rather than 404 on a query param.
	root := r.URL.Query().Get("root")
	if root != "" && cfg.Find(root) == nil {
		root = ""
	}

	var docs []index.Doc
	var err error
	if q != "" {
		docs, err = s.store.Search(r.Context(), q, root)
	} else {
		docs, err = s.store.ListDocs(r.Context(), root)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	counts, err := s.store.AllRootStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pills := make([]rootPill, 0, len(cfg.Roots))
	for _, rt := range cfg.Roots {
		c := counts[rt.Name] // zero value keeps a pill for still-empty roots
		pills = append(pills, rootPill{Name: rt.Name, Unread: c.Unread, Total: c.Total})
	}

	renderInbox(w, inboxData{Title: "markroom", Query: q, Root: root, Roots: pills, Docs: docs})
}

func (s *Server) handleDoc(w http.ResponseWriter, r *http.Request) {
	root := s.cfg.Load().Find(r.PathValue("root"))
	if root == nil {
		http.NotFound(w, r)
		return
	}
	rel := cleanSubpath(r.PathValue("path"))
	full := filepath.Join(root.Path, filepath.FromSlash(rel))
	inside, err := filepath.Rel(root.Path, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}

	if !index.IsMarkdown(full) {
		if !assetExts[strings.ToLower(filepath.Ext(full))] {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, full)
		return
	}

	content, err := os.ReadFile(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Index may lag the disk; upsert so what we show is what we record.
	if info, statErr := os.Stat(full); statErr == nil {
		_ = s.store.UpsertDoc(r.Context(), root.Name, rel, content, info.ModTime(), info.Size())
	}
	doc, err := s.store.GetDoc(r.Context(), root.Name, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := md.Convert([]byte(index.StripFrontmatter(string(content))), &buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Opening a document is reading it.
	sum := sha256.Sum256(content)
	_ = s.store.MarkRead(r.Context(), doc.ID, hex.EncodeToString(sum[:]))

	renderDoc(w, doc, buf.String())
}

func (s *Server) handleUnread(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	doc, err := s.store.GetDoc(r.Context(), r.Form.Get("root"), r.Form.Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.MarkUnread(r.Context(), doc.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	s.reloadAndScan(r.Context())
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "max-age=300")
	fmt.Fprint(w, fullCSS())
}

// cleanSubpath normalizes a URL sub-path to forward slashes with no leading slash.
func cleanSubpath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	return p
}

// hostGuard rejects requests whose Host header doesn't match how this server
// is legitimately reached. This blocks DNS rebinding: a malicious website
// resolving its own domain to 127.0.0.1 sends its domain as the Host header,
// which lets it read local documents cross-origin unless we refuse it here.
func hostGuard(addr string, extra []string, next http.Handler) http.Handler {
	allowed := map[string]bool{"localhost": true}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		h = strings.ToLower(strings.Trim(h, "[]"))
		if h != "" && h != "0.0.0.0" && h != "::" {
			allowed[h] = true
		}
	}
	for _, h := range extra {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.ToLower(strings.Trim(host, "[]"))
		ok := allowed[host] ||
			strings.HasSuffix(host, ".ts.net") // tailscale serve / MagicDNS names
		if !ok {
			if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
				ok = true
			}
		}
		if !ok {
			http.Error(w,
				fmt.Sprintf("markroom: refusing unrecognized Host %q (DNS-rebinding protection). "+
					"If this is a name you trust, restart with: markroom serve --allow-host %s", host, host),
				http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
