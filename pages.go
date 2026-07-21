package main

import (
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

var tpl = template.Must(template.New("").Funcs(template.FuncMap{
	"timeago":  timeAgo,
	"readmins": readMins,
	"raw":      func(s string) template.HTML { return template.HTML(s) },
}).Parse(layoutTpl + inboxTpl + docTpl))

const layoutTpl = `
{{define "head"}}<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{.Title}}</title>
<link rel="stylesheet" href="/app.css">
{{end}}`

const inboxTpl = `
{{define "inbox"}}{{template "head" .}}
<body class="inbox">
<header class="bar">
  <a class="brand" href="/">markroom</a>
  <form class="search" action="/" method="get">
    <input type="search" name="q" value="{{.Query}}" placeholder="search everything…" autocomplete="off">
  </form>
  <form action="/api/rescan" method="post"><button class="ghost" title="rescan roots">↻</button></form>
</header>
<main>
{{if .Query}}<p class="meta">{{len .Docs}} result(s) for “{{.Query}}” — <a href="/">clear</a></p>{{end}}
{{if not .Docs}}<p class="empty">{{if .Query}}Nothing matched.{{else}}Oh, hi Mark. Nothing indexed yet — register a directory with <code>markroom add &lt;dir&gt;</code>.{{end}}</p>{{end}}
<ul class="docs">
{{range .Docs}}
  <li class="{{.Status}}">
    <a class="doc" href="/d/{{.Root}}/{{.RelPath}}">
      <span class="row1">
        <span class="title">{{.Title}}</span>
        {{if eq .Status "new"}}<span class="badge new">new</span>{{end}}
        {{if eq .Status "updated"}}<span class="badge updated">updated</span>{{end}}
      </span>
      <span class="row2">
        <span class="root">{{.Root}}</span>
        <span class="path">{{.RelPath}}</span>
        <span class="dot">·</span> {{timeago .MTime}}
        <span class="dot">·</span> {{readmins .Words}} min
      </span>
      {{if .Snippet}}<span class="snippet">{{raw .Snippet}}</span>{{end}}
    </a>
  </li>
{{end}}
</ul>
</main>
</body></html>{{end}}`

const docTpl = `
{{define "doc"}}{{template "head" .}}
<body class="reader">
<header class="bar">
  <a class="brand" href="/">← markroom</a>
  <span class="docmeta">
    <span class="root">{{.Doc.Root}}</span>
    <span class="path">{{.Doc.RelPath}}</span>
    <span class="dot">·</span> {{timeago .Doc.MTime}}
    <span class="dot">·</span> {{readmins .Doc.Words}} min
  </span>
  <form action="/api/unread" method="post">
    <input type="hidden" name="root" value="{{.Doc.Root}}">
    <input type="hidden" name="path" value="{{.Doc.RelPath}}">
    <button class="ghost" title="mark unread">mark unread</button>
  </form>
</header>
<main><article>{{raw .HTML}}</article></main>
</body></html>{{end}}`

type inboxData struct {
	Title string
	Query string
	Docs  []Doc
}

type docData struct {
	Title string
	Doc   *Doc
	HTML  string
}

func renderInbox(w http.ResponseWriter, query string, docs []Doc) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tpl.ExecuteTemplate(w, "inbox", inboxData{Title: "markroom", Query: query, Docs: docs})
	if err != nil {
		slog.Error("render inbox", "err", err)
	}
}

func renderDoc(w http.ResponseWriter, doc *Doc, htmlBody string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	err := tpl.ExecuteTemplate(w, "doc", docData{Title: doc.Title + " — markroom", Doc: doc, HTML: htmlBody})
	if err != nil {
		slog.Error("render doc", "err", err)
	}
}

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

const baseCSS = `
:root{
  --bg:#ffffff; --fg:#1a1a1a; --muted:#6b7280; --line:#e5e7eb;
  --accent:#2563eb; --accent-bg:#eff6ff; --mark:#fef08a;
  --new:#16a34a; --updated:#d97706; --code-bg:#f6f8fa;
}
@media (prefers-color-scheme: dark){
  :root{
    --bg:#111418; --fg:#e6e6e6; --muted:#9ca3af; --line:#2a2f36;
    --accent:#60a5fa; --accent-bg:#1e293b; --mark:#854d0e;
    --new:#4ade80; --updated:#fbbf24; --code-bg:#1b1f24;
  }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);
  font:16px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
a{color:var(--accent);text-decoration:none}
main{max-width:72ch;margin:0 auto;padding:1rem 1.25rem 4rem}

.bar{position:sticky;top:0;display:flex;align-items:center;gap:.75rem;
  padding:.6rem 1rem;background:var(--bg);border-bottom:1px solid var(--line);z-index:10}
.brand{font-weight:700;color:var(--fg);white-space:nowrap}
.search{flex:1}
.search input{width:100%;padding:.45rem .7rem;border:1px solid var(--line);
  border-radius:.5rem;background:var(--bg);color:var(--fg);font-size:.95rem}
.ghost{border:1px solid var(--line);background:none;color:var(--muted);
  border-radius:.5rem;padding:.4rem .7rem;cursor:pointer;font-size:.85rem;white-space:nowrap}
.ghost:hover{color:var(--fg);border-color:var(--muted)}
.docmeta{flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;
  color:var(--muted);font-size:.85rem}

.meta,.empty{color:var(--muted)}
ul.docs{list-style:none;margin:0;padding:0}
ul.docs li{border-bottom:1px solid var(--line)}
a.doc{display:block;padding:.8rem .25rem;color:inherit}
a.doc:hover .title{color:var(--accent)}
.row1{display:flex;align-items:baseline;gap:.5rem}
.title{font-weight:600}
li.read .title{font-weight:400;color:var(--muted)}
.badge{font-size:.7rem;font-weight:700;text-transform:uppercase;letter-spacing:.03em;
  padding:.1rem .4rem;border-radius:.4rem}
.badge.new{color:var(--new);border:1px solid var(--new)}
.badge.updated{color:var(--updated);border:1px solid var(--updated)}
.row2{display:block;margin-top:.15rem;color:var(--muted);font-size:.82rem}
.root{color:var(--accent);background:var(--accent-bg);padding:.05rem .4rem;
  border-radius:.35rem;font-size:.78rem}
.path{word-break:break-all}
.dot{opacity:.5;margin:0 .15rem}
.snippet{display:block;margin-top:.3rem;color:var(--muted);font-size:.88rem}
mark{background:var(--mark);color:inherit;border-radius:.15rem;padding:0 .1rem}

/* ---------- reading view ---------- */
article{font-family:Charter,Georgia,"Times New Roman",serif;
  font-size:1.08rem;line-height:1.68;padding-top:1rem;
  overflow-wrap:break-word}
article h1,article h2,article h3,article h4{
  font-family:system-ui,-apple-system,"Segoe UI",sans-serif;line-height:1.25}
article h1{font-size:1.7rem;margin:1.2rem 0 .6rem}
article h2{font-size:1.35rem;margin-top:2.2rem;border-bottom:1px solid var(--line);
  padding-bottom:.25rem}
article h3{font-size:1.1rem;margin-top:1.8rem}
article img{max-width:100%}
article blockquote{margin:1rem 0;padding:.1rem 1rem;border-left:3px solid var(--line);
  color:var(--muted)}
article code{font:.85em/1.5 ui-monospace,"Cascadia Code",Consolas,monospace;
  background:var(--code-bg);padding:.12em .35em;border-radius:.3rem}
article pre{background:var(--code-bg);padding:.9rem 1rem;border-radius:.6rem;
  overflow-x:auto;line-height:1.5}
article pre code{background:none;padding:0;font-size:.85rem}
article .chroma{background:var(--code-bg) !important}
article table{border-collapse:collapse;display:block;overflow-x:auto;font-size:.92em;
  font-family:system-ui,-apple-system,"Segoe UI",sans-serif}
article th,article td{border:1px solid var(--line);padding:.35rem .7rem;text-align:left}
article th{background:var(--code-bg)}
article hr{border:none;border-top:1px solid var(--line);margin:2rem 0}
article input[type=checkbox]{margin-right:.4rem}

@media (max-width:600px){
  main{padding:.75rem .9rem 3rem}
  article{font-size:1.02rem}
  .docmeta .path{display:none}
}
`
