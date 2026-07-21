package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	store := testStore(t)
	dir := t.TempDir()
	cfg := &Config{Roots: []Root{{Name: "docs", Path: dir}}}
	srv := NewServer(store, cfg)
	srv.reloadConfig = func() (*Config, error) { return cfg, nil }
	return srv, dir
}

func get(t *testing.T, client *http.Client, url string, wantStatus int, wantContains ...string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("GET %s = %d, want %d", url, resp.StatusCode, wantStatus)
	}
	for _, want := range wantContains {
		if !strings.Contains(string(body), want) {
			t.Errorf("GET %s: body missing %q", url, want)
		}
	}
	return string(body)
}

func TestServerEndToEnd(t *testing.T) {
	srv, dir := newTestServer(t)
	writeFile(t, filepath.Join(dir, "spec.md"), "# The Spec\n\nhello body text")
	writeFile(t, filepath.Join(dir, "img.png"), "not-really-a-png")
	writeFile(t, filepath.Join(dir, "tool.exe"), "MZ")
	srv.reloadAndScan()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// Inbox lists the doc as unread.
	get(t, client, ts.URL+"/", http.StatusOK, "The Spec", "badge new")

	// Opening the doc renders markdown and marks it read.
	get(t, client, ts.URL+"/d/docs/spec.md", http.StatusOK, "<h1", "hello body text")
	doc, err := srv.store.GetDoc("docs", "spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Status != "read" {
		t.Fatalf("after open, status = %q, want read", doc.Status)
	}

	// Whitelisted assets are served; other extensions are not.
	get(t, client, ts.URL+"/d/docs/img.png", http.StatusOK)
	get(t, client, ts.URL+"/d/docs/tool.exe", http.StatusNotFound)

	// Path traversal is rejected.
	get(t, client, ts.URL+"/d/docs/..%2F..%2Fsecret.md", http.StatusNotFound)

	// Unknown root is rejected.
	get(t, client, ts.URL+"/d/nope/spec.md", http.StatusNotFound)

	// Search hits body text.
	get(t, client, ts.URL+"/?q=hello", http.StatusOK, "result(s)", "The Spec")

	// Mark-unread flips the doc back (client follows the redirect home).
	resp, err := client.PostForm(ts.URL+"/api/unread",
		url.Values{"root": {"docs"}, "path": {"spec.md"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unread POST final status = %d, want 200", resp.StatusCode)
	}
	doc, _ = srv.store.GetDoc("docs", "spec.md")
	if doc.Status != "new" {
		t.Fatalf("after unread, status = %q, want new", doc.Status)
	}

	// Rescan endpoint picks up newly written files.
	writeFile(t, filepath.Join(dir, "fresh.md"), "# Fresh\n\njust arrived")
	resp, err = client.Post(ts.URL+"/api/rescan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	get(t, client, ts.URL+"/", http.StatusOK, "Fresh")

	// Stylesheet includes both base and chroma rules.
	get(t, client, ts.URL+"/app.css", http.StatusOK, ".chroma", "--bg")
}

func TestHostGuard(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	tests := []struct {
		name   string
		addr   string
		extra  []string
		host   string
		status int
	}{
		{"localhost", "127.0.0.1:8383", nil, "localhost:8383", http.StatusOK},
		{"loopback v4", "127.0.0.1:8383", nil, "127.0.0.1:8383", http.StatusOK},
		{"loopback v6", "127.0.0.1:8383", nil, "[::1]:8383", http.StatusOK},
		{"other loopback ip", "127.0.0.1:8383", nil, "127.0.0.2:8383", http.StatusOK},
		{"tailscale magicdns", "127.0.0.1:8383", nil, "desktop.tailnet.ts.net", http.StatusOK},
		{"rebinding domain", "127.0.0.1:8383", nil, "evil.com:8383", http.StatusForbidden},
		{"rebinding no port", "127.0.0.1:8383", nil, "evil.com", http.StatusForbidden},
		{"ts.net lookalike", "127.0.0.1:8383", nil, "evilts.net", http.StatusForbidden},
		{"lan ip not allowed", "127.0.0.1:8383", nil, "192.168.1.10:8383", http.StatusForbidden},
		{"explicitly allowed host", "127.0.0.1:8383", []string{"docs.internal"}, "docs.internal:8383", http.StatusOK},
		{"allowed host case-insensitive", "127.0.0.1:8383", []string{"Docs.Internal"}, "docs.internal", http.StatusOK},
		{"bound host allowed", "192.168.1.10:8383", nil, "192.168.1.10:8383", http.StatusOK},
		{"wildcard bind allows localhost", "0.0.0.0:8383", nil, "localhost:8383", http.StatusOK},
		{"wildcard bind rejects arbitrary", "0.0.0.0:8383", nil, "evil.com:8383", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := hostGuard(tt.addr, tt.extra, ok)
			req := httptest.NewRequest("GET", "http://placeholder/", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("addr=%s host=%s: got %d, want %d", tt.addr, tt.host, rec.Code, tt.status)
			}
		})
	}
}

func TestCleanSubpath(t *testing.T) {
	tests := []struct{ in, want string }{
		{"specs/payments.md", "specs/payments.md"},
		{"/leading/slash.md", "leading/slash.md"},
		{"///many.md", "many.md"},
		{`back\slashes\win.md`, "back/slashes/win.md"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := cleanSubpath(tt.in); got != tt.want {
			t.Errorf("cleanSubpath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
