package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
)

// docChromeLines is the fixed height around the viewport: the title line,
// the metadata line, and the footer.
const docChromeLines = 3

// renderMarkdown wraps the stored body to the current width with Glamour.
// Falls back to the raw markdown if rendering fails — a readable document
// beats an error screen.
func (m model) renderMarkdown() string {
	width := m.width
	if width == 0 {
		width = 80
	}
	wrap := max(20, min(width-2, 100))
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(m.mdStyle),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		return m.rawBody
	}
	out, err := r.Render(m.rawBody)
	if err != nil {
		return m.rawBody
	}
	return out
}

func (m model) updateDoc(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "q", "esc":
			m.screen = screenInbox
			return m, m.refreshCmd() // pick up the just-changed read state
		case "u":
			if m.doc != nil {
				return m, m.unreadCmd(m.doc.ID)
			}
			return m, nil
		case "g", "home":
			m.vp.GotoTop()
			return m, nil
		case "G", "end":
			m.vp.GotoBottom()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

func (m model) viewDoc() string {
	if m.doc == nil {
		return ""
	}
	title := titleStyle.Render(m.doc.Title)
	line := fmt.Sprintf("%s/%s · %s", m.doc.Root, m.doc.RelPath, statusLabel(m.doc.Status))
	if origin := m.originOf(m.doc.Root); origin != "" {
		line += " · from " + origin
	}
	meta := titleMetaStyle.Render(line)
	footer := hintStyle.Render(fmt.Sprintf("%3.0f%% · esc back · u mark unread · ctrl+c quit",
		m.vp.ScrollPercent()*100))
	if m.status != "" {
		footer = errStyle.Render(m.status)
	}
	var b strings.Builder
	b.WriteString(title)
	b.WriteByte('\n')
	b.WriteString(meta)
	b.WriteByte('\n')
	b.WriteString(m.vp.View())
	b.WriteByte('\n')
	b.WriteString(footer)
	return b.String()
}

// statusLabel names the state this document was in when opened.
func statusLabel(status string) string {
	switch status {
	case "new":
		return "new"
	case "updated":
		return "updated"
	default:
		return "read"
	}
}
