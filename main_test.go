package main

import (
	"path/filepath"
	"testing"

	"github.com/ahaley/markroom/internal/config"
)

// TestDefaultRootName guards how `markroom add <dir>` names a root when
// --name is omitted, including that the derived name always survives the
// validator cmdAdd runs it through. (Slugify itself is tested in config.)
func TestDefaultRootName(t *testing.T) {
	tests := []struct{ dir, want string }{
		{filepath.FromSlash("/home/me/My Docs"), "my-docs"},
		{filepath.FromSlash("/src/CBM.Gestalt"), "cbm-gestalt"},
		{filepath.FromSlash("/src/日本語"), "root"},
	}
	for _, tt := range tests {
		got := config.Slugify(filepath.Base(tt.dir))
		if got != tt.want {
			t.Errorf("default name for %q = %q, want %q", tt.dir, got, tt.want)
		}
		if err := config.ValidRootName(got); err != nil {
			t.Errorf("derived name %q rejected by ValidRootName: %v", got, err)
		}
	}
}
