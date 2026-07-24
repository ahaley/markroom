// Package format holds small presentation helpers shared by the web and
// terminal UIs, so both surfaces label documents the same way.
package format

import (
	"fmt"
	"time"
)

// TimeAgo renders a timestamp as a compact relative age ("3h ago").
func TimeAgo(t time.Time) string {
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

// ReadMins estimates reading time in minutes at ~220 words per minute,
// never reporting less than one minute.
func ReadMins(words int) int {
	return max(words/220, 1)
}
