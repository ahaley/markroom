package web

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"

	"github.com/ahaley/markroom/internal/index"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

//go:embed static/app.css
var baseCSS string

var tpl = template.Must(template.New("").Funcs(template.FuncMap{
	"timeago":  timeAgo,
	"readmins": readMins,
	"raw":      func(s string) template.HTML { return template.HTML(s) },
}).ParseFS(templateFS, "templates/*.tmpl"))

type inboxData struct {
	Title string
	Query string
	Docs  []index.Doc
}

type docData struct {
	Title string
	Doc   *index.Doc
	HTML  string
}

func renderInbox(w http.ResponseWriter, query string, docs []index.Doc) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tpl.ExecuteTemplate(w, "inbox", inboxData{Title: "markroom", Query: query, Docs: docs})
	if err != nil {
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

func timeAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func readMins(words int) int {
	m := words / 220
	if m < 1 {
		m = 1
	}
	return m
}
