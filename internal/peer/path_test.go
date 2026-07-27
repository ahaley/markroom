package peer

import (
	"path/filepath"
	"testing"
)

func TestSafeRelPath(t *testing.T) {
	ok := []string{
		"spec.md",
		"specs/payments.md",
		"a/b/c/deep.markdown",
		"img/flow.png",
		"notes/my notes.md", // spaces are ordinary
		"data.csv",
	}
	for _, p := range ok {
		if _, err := SafeRelPath(p); err != nil {
			t.Errorf("SafeRelPath(%q) = %v, want nil", p, err)
		}
	}

	bad := []struct{ path, why string }{
		{"", "empty"},
		{"/etc/passwd.md", "absolute"},
		{"../escape.md", "parent segment"},
		{"a/../../escape.md", "parent segment mid-path"},
		{`a\b.md`, "backslash"},
		{`C:\windows\x.md`, "windows absolute"},
		{"a//b.md", "empty segment"},
		{"./a.md", "dot segment"},
		{"a\x00b.md", "control character"},
		{"tool.exe", "disallowed extension"},
		{"noext", "no extension"},
		{"CON.md", "reserved device name"},
		{"lpt1.md", "reserved device name, lowercase"},
		{"sub/nul.md", "reserved device name in a subdirectory"},
		{"trailing./x.md", "segment ending in a dot"},
		{"trailing /x.md", "segment ending in a space"},
		{"x.md:stream", "alternate data stream"},
		{"a<b>.md", "reserved characters"},
	}
	for _, tt := range bad {
		if _, err := SafeRelPath(tt.path); err == nil {
			t.Errorf("SafeRelPath(%q) = nil, want an error (%s)", tt.path, tt.why)
		}
	}

	// Depth is bounded.
	deep := ""
	for i := 0; i < maxSegments+1; i++ {
		deep += "d/"
	}
	if _, err := SafeRelPath(deep + "x.md"); err == nil {
		t.Error("an over-deep path was accepted")
	}
}

func TestValidRemoteName(t *testing.T) {
	for _, s := range []string{"laptop", "work-desktop", "m1", "Server_2"} {
		if err := ValidRemoteName(s); err != nil {
			t.Errorf("ValidRemoteName(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", ".", "..", "a/b", `a\b`, "a:b", "trailing.", " lead", "a\x00b"} {
		if err := ValidRemoteName(s); err == nil {
			t.Errorf("ValidRemoteName(%q) = nil, want an error", s)
		}
	}
}

func TestInsideRoot(t *testing.T) {
	root := filepath.FromSlash("/srv/cache/laptop/notes")
	if _, err := InsideRoot(root, "specs/pay.md"); err != nil {
		t.Errorf("InsideRoot on a plain path = %v", err)
	}
	// SafeRelPath already rejects these; InsideRoot is the second lock on the
	// same door, so it has to hold on its own.
	for _, rel := range []string{"../outside.md", "a/../../outside.md"} {
		if _, err := InsideRoot(root, rel); err == nil {
			t.Errorf("InsideRoot(%q) = nil, want an error", rel)
		}
	}
}

func TestValidatePeerURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"http://laptop:8383", "http://laptop:8383"},
		{"https://laptop.tailnet.ts.net/", "https://laptop.tailnet.ts.net"},
		{"http://127.0.0.1:8383/", "http://127.0.0.1:8383"},
	}
	for _, tt := range tests {
		got, err := ValidatePeerURL(tt.in)
		if err != nil || got != tt.want {
			t.Errorf("ValidatePeerURL(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, s := range []string{"", "laptop:8383", "file:///etc", "ftp://host", "http://"} {
		if _, err := ValidatePeerURL(s); err == nil {
			t.Errorf("ValidatePeerURL(%q) = nil, want an error", s)
		}
	}
}
