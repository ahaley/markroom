package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
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

func cmdServe(args []string) error {
	addr := "127.0.0.1:8383"
	var allowHosts []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--addr" && i+1 < len(args):
			addr = args[i+1]
			i++
		case args[i] == "--allow-host" && i+1 < len(args):
			for _, h := range strings.Split(args[i+1], ",") {
				if h = strings.TrimSpace(h); h != "" {
					allowHosts = append(allowHosts, h)
				}
			}
			i++
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	initial, err := loadConfig()
	if err != nil {
		return err
	}
	var cfg atomic.Pointer[Config]
	cfg.Store(initial)
	db, err := openDB()
	if err != nil {
		return err
	}
	defer db.Close()

	fmt.Println("markroom: initial scan…")
	scanAll(db, cfg.Load())

	reloadAndScan := func() {
		if fresh, err := loadConfig(); err == nil {
			cfg.Store(fresh)
		}
		scanAll(db, cfg.Load())
	}

	// Periodic rescan keeps the index tracking whatever agents write.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for range t.C {
			reloadAndScan()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		handleInbox(db, w, r)
	})
	mux.HandleFunc("GET /d/{root}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		handleDoc(db, cfg.Load(), w, r)
	})
	mux.HandleFunc("POST /api/unread", func(w http.ResponseWriter, r *http.Request) {
		handleUnread(db, w, r)
	})
	mux.HandleFunc("POST /api/rescan", func(w http.ResponseWriter, r *http.Request) {
		reloadAndScan()
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /app.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "max-age=300")
		fmt.Fprint(w, fullCSS())
	})

	fmt.Printf("markroom: oh, hi — reading at http://%s\n", addr)
	fmt.Println("markroom: for your phone over Tailscale:  tailscale serve --bg http://" + addr)
	return http.ListenAndServe(addr, hostGuard(addr, allowHosts, mux))
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

func handleInbox(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var docs []Doc
	var err error
	if q != "" {
		docs, err = searchDocs(db, q)
	} else {
		docs, err = listDocs(db)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	renderInbox(w, q, docs)
}

func handleDoc(db *sql.DB, cfg *Config, w http.ResponseWriter, r *http.Request) {
	root := cfg.find(r.PathValue("root"))
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

	if !isMarkdown(full) {
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
		_ = upsertDoc(db, root.Name, rel, content, info.ModTime(), info.Size())
	}
	doc, err := getDoc(db, root.Name, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	if err := md.Convert([]byte(stripFrontmatter(string(content))), &buf); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Opening a document is reading it.
	sum := sha256.Sum256(content)
	_ = markRead(db, doc.ID, hex.EncodeToString(sum[:]))

	renderDoc(w, doc, buf.String())
}

func handleUnread(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	doc, err := getDoc(db, r.Form.Get("root"), r.Form.Get("path"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := markUnread(db, doc.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// cleanSubpath normalizes a URL sub-path to forward slashes with no leading slash.
func cleanSubpath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "/") {
		p = p[1:]
	}
	return p
}

var chromaCSS string

func fullCSS() string {
	if chromaCSS == "" {
		f := chromahtml.New(chromahtml.WithClasses(true))
		var light, dark strings.Builder
		_ = f.WriteCSS(&light, styles.Get("github"))
		_ = f.WriteCSS(&dark, styles.Get("github-dark"))
		chromaCSS = light.String() +
			"\n@media (prefers-color-scheme: dark){\n" + dark.String() + "\n}\n"
	}
	return baseCSS + chromaCSS
}
