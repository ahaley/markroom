package peer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Extensions a mirror is willing to write. Markdown is the point; the rest
// are the attachments documents reference, and must match what the serving
// side is willing to hand out.
var allowedExts = map[string]bool{
	".md": true, ".markdown": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".txt": true, ".json": true, ".csv": true, ".pdf": true,
}

// Names that would break, or mean something surprising, as a directory
// component on Windows.
var reservedNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

const (
	maxPathLen  = 1024
	maxSegments = 32
	maxNameLen  = 64
)

// SafeRelPath validates a slash-separated path taken from a peer's manifest
// before it is used to write a file. A peer is not trusted: this is the last
// line between a hostile manifest and an arbitrary write, so it is
// deliberately stricter than any one operating system requires.
func SafeRelPath(p string) (string, error) {
	switch {
	case p == "":
		return "", fmt.Errorf("empty path")
	case len(p) > maxPathLen:
		return "", fmt.Errorf("path longer than %d bytes", maxPathLen)
	case strings.HasPrefix(p, "/"):
		return "", fmt.Errorf("absolute path %q", p)
	case strings.Contains(p, `\`):
		return "", fmt.Errorf("path %q contains a backslash", p)
	}
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return "", fmt.Errorf("path %q contains a control character", p)
		}
	}
	segs := strings.Split(p, "/")
	if len(segs) > maxSegments {
		return "", fmt.Errorf("path %q is more than %d segments deep", p, maxSegments)
	}
	for _, s := range segs {
		if err := safeSegment(s); err != nil {
			return "", fmt.Errorf("in path %q: %w", p, err)
		}
	}
	if ext := strings.ToLower(filepath.Ext(p)); !allowedExts[ext] {
		return "", fmt.Errorf("path %q has a disallowed extension %q", p, ext)
	}
	return p, nil
}

func safeSegment(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("empty path segment")
	case s == "." || s == "..":
		return fmt.Errorf("path segment %q", s)
	case strings.ContainsAny(s, `:<>"|?*`):
		return fmt.Errorf("path segment %q contains a reserved character", s)
	// Windows silently strips these, so "evil. " and "evil" would land in the
	// same place — an easy way to smuggle a name past a uniqueness check.
	case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " "):
		return fmt.Errorf("path segment %q ends in a dot or space", s)
	case strings.HasPrefix(s, " "):
		return fmt.Errorf("path segment %q starts with a space", s)
	}
	base, _, _ := strings.Cut(strings.ToLower(s), ".")
	if reservedNames[base] {
		return fmt.Errorf("path segment %q is a reserved device name", s)
	}
	return nil
}

// ValidRemoteName checks a server or root name from a peer's manifest. These
// are shown to the reader and folded into local root names, so they must be
// short, printable, and free of anything that means something in a path.
func ValidRemoteName(s string) error {
	switch {
	case s == "":
		return fmt.Errorf("empty name")
	case len(s) > maxNameLen:
		return fmt.Errorf("name %q is longer than %d characters", s, maxNameLen)
	case s == "." || s == "..":
		return fmt.Errorf("name %q is reserved", s)
	case strings.ContainsAny(s, `/\:<>"|?*`):
		return fmt.Errorf("name %q contains a reserved character", s)
	case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " ") || strings.HasPrefix(s, " "):
		return fmt.Errorf("name %q has leading or trailing whitespace or a trailing dot", s)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("name %q contains a control character", s)
		}
	}
	return nil
}

// InsideRoot resolves rel against root and confirms the result did not escape
// it — the same lexical containment check the reading server applies to URLs,
// repeated here because SafeRelPath alone should never be the only guard.
func InsideRoot(root, rel string) (string, error) {
	full := filepath.Join(root, filepath.FromSlash(rel))
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes %q", rel, root)
	}
	return full, nil
}
