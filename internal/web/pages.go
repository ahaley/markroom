package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/ahaley/markroom/internal/format"
	"github.com/ahaley/markroom/internal/index"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed static/app.css
var baseCSS string

var tpl = template.Must(template.New("").Funcs(template.FuncMap{
	"timeago":  format.TimeAgo,
	"readmins": format.ReadMins,
	"raw":      func(s string) template.HTML { return template.HTML(s) },
}).ParseFS(templateFS, "templates/*.tmpl"))

// rootPill is one entry in the inbox root filter bar. Origin is the machine a
// mirrored root came from, empty for local roots.
type rootPill struct {
	Name   string
	Origin string
	Unread int
	Total  int
}

// docRow is one inbox row: an indexed document plus where it came from.
// index.Doc is embedded so the template keeps reaching its fields directly.
type docRow struct {
	index.Doc
	Origin string
}

// peerStatus is one line of "this peer is not answering", shown only when
// something is actually wrong.
type peerStatus struct {
	Name string
	Err  string
	Last time.Time
}

type inboxData struct {
	Title    string
	Query    string
	Root     string     // active root filter, "" = none
	Roots    []rootPill // all registered roots, config order
	Docs     []docRow
	Peers    []peerStatus // only the unhealthy ones
	HasPeers bool         // any peers at all, healthy or not
}

type docData struct {
	Title  string
	Doc    *index.Doc
	HTML   string
	Origin string // machine the document was written on, "" if local
}

// contentPolicy is the CSP applied to every page that renders document
// content. markdown is rendered with raw HTML allowed, which was fine when
// every document came off this machine's own disk — with peering it no
// longer is, so the pages that embed it refuse to run script. There is no
// JavaScript in this UI, so nothing legitimate is lost. Remote images stay
// permitted because README-style badges are half the reason raw HTML is on.
const contentPolicy = "default-src 'self'; script-src 'none'; " +
	"img-src 'self' data: https:; style-src 'self' 'unsafe-inline'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func writeHTMLHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", contentPolicy)
	w.Header().Set("Referrer-Policy", "no-referrer")
}

func renderInbox(w http.ResponseWriter, d inboxData) {
	writeHTMLHeaders(w)
	if err := tpl.ExecuteTemplate(w, "inbox", d); err != nil {
		slog.Error("render inbox", "err", err)
	}
}

func renderDoc(w http.ResponseWriter, doc *index.Doc, htmlBody, origin string) {
	writeHTMLHeaders(w)
	err := tpl.ExecuteTemplate(w, "doc", docData{
		Title:  doc.Title + " — markroom",
		Doc:    doc,
		HTML:   htmlBody,
		Origin: origin,
	})
	if err != nil {
		slog.Error("render doc", "err", err)
	}
}

// fullCSS lazily renders the chroma stylesheets exactly once; concurrent
// /app.css requests share the same computation.
var fullCSS = sync.OnceValue(func() string {
	f := chromahtml.New(chromahtml.WithClasses(true))
	var light, dark strings.Builder
	_ = f.WriteCSS(&light, styles.Get("github"))
	_ = f.WriteCSS(&dark, styles.Get("github-dark"))
	return baseCSS + light.String() +
		"\n@media (prefers-color-scheme: dark){\n" + dark.String() + "\n}\n"
})
