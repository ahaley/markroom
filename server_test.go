package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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
