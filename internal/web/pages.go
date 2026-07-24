package web

import (
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"

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

// rootPill is one entry in the inbox root filter bar.
type rootPill struct {
	Name   string
	Unread int
	Total  int
}

type inboxData struct {
	Title string
	Query string
	Root  string     // active root filter, "" = none
	Roots []rootPill // all registered roots, config order
	Docs  []index.Doc
}

type docData struct {
	Title string
	Doc   *index.Doc
	HTML  string
}

func renderInbox(w http.ResponseWriter, d inboxData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tpl.ExecuteTemplate(w, "inbox", d); err != nil {
		slog.Error("render inbox", "err", err)
	}
}

func renderDoc(w http.ResponseWriter, doc *index.Doc, htmlBody string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tpl.ExecuteTemplate(w, "doc", docData{Title: doc.Title + " — markroom", Doc: doc, HTML: htmlBody})
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
