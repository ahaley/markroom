package main

import "testing"

func TestSlugify(t *testing.T) {
	tests := []struct{ in, want string }{
		{"My Docs", "my-docs"},
		{"CBM.Gestalt", "cbm-gestalt"},
		{"already-fine_slug", "already-fine_slug"},
		{"--trim--", "trim"},
		{"日本語", "root"},
		{"", "root"},
	}
	for _, tt := range tests {
		if got := slugify(tt.in); got != tt.want {
			t.Errorf("slugify(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
