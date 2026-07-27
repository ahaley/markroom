package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ahaley/markroom/internal/format"
	"github.com/ahaley/markroom/internal/index"
)

// inboxChromeLines is the fixed height of everything around the list:
// title bar, root pills, search line, and the footer hints.
const inboxChromeLines = 4

var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62")).Padding(0, 1)
	titleMetaStyle  = lipgloss.NewStyle().Faint(true)
	pillStyle       = lipgloss.NewStyle().Padding(0, 1).Faint(true)
	pillActiveStyle = lipgloss.NewStyle().Padding(0, 1).Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("62"))
	unreadCntStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	hintStyle       = lipgloss.NewStyle().Faint(true)
	errStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

// docItem adapts an index.Doc to the bubbles list. origin names the machine a
// mirrored document was written on, and is empty for local roots.
type docItem struct {
	doc    index.Doc
	origin string
}

// statusBadge marks unread work the way the web inbox does; read docs get
// aligning whitespace instead of a glyph.
func statusBadge(status string) string {
	switch status {
	case "new":
		return "● "
	case "updated":
		return "◐ "
	default:
		return "  "
	}
}

func (i docItem) Title() string {
	return statusBadge(i.doc.Status) + i.doc.Title
}

func (i docItem) Description() string {
	// Search results carry an FTS snippet (HTML for the web UI); it beats the
	// metadata line as context for why this document matched.
	if i.doc.Snippet != "" {
		s := strings.NewReplacer("<mark>", "", "</mark>", "", "\n", " ").Replace(i.doc.Snippet)
		return "  " + s
	}
	origin := ""
	if i.origin != "" {
		origin = "↗" + i.origin + " "
	}
	return fmt.Sprintf("  %s%s/%s · %s · %d min read",
		origin, i.doc.Root, i.doc.RelPath, format.TimeAgo(i.doc.MTime), format.ReadMins(i.doc.Words))
}

func (i docItem) FilterValue() string { return i.doc.Title + " " + i.doc.RelPath }

func (m model) updateInbox(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, isKey := msg.(tea.KeyMsg)
	if !isKey {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	// While the search input is focused it owns the keyboard.
	if m.searching {
		switch key.String() {
		case "enter":
			m.searching = false
			m.search.Blur()
			m.query = strings.TrimSpace(m.search.Value())
			return m, m.refreshCmd()
		case "esc":
			m.searching = false
			m.search.Blur()
			m.search.SetValue(m.query) // keep the input mirroring the active query
			return m, nil
		default:
			var cmd tea.Cmd
			m.search, cmd = m.search.Update(msg)
			return m, cmd
		}
	}

	switch key.String() {
	case "q":
		return m, tea.Quit
	case "/":
		m.searching = true
		m.search.Focus()
		return m, nil
	case "esc":
		if m.query != "" { // clear an active search, back to the full inbox
			m.query = ""
			m.search.SetValue("")
			return m, m.refreshCmd()
		}
		return m, nil
	case "tab", "shift+tab":
		n := len(m.cfg.Roots) + 1 // "all" plus each root
		if key.String() == "tab" {
			m.rootIdx = (m.rootIdx + 1) % n
		} else {
			m.rootIdx = (m.rootIdx + n - 1) % n
		}
		return m, m.refreshCmd()
	case "r":
		if m.scanning {
			return m, nil
		}
		m.scanning = true
		return m, tea.Batch(m.scanCmd(), m.spin.Tick)
	case "s":
		if m.scanning || len(m.cfg.Peers) == 0 {
			return m, nil
		}
		m.scanning = true
		return m, tea.Batch(m.syncCmd(), m.spin.Tick)
	case "enter":
		if it, ok := m.list.SelectedItem().(docItem); ok {
			return m, m.openCmd(it.doc)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m model) viewInbox() string {
	var b strings.Builder
	b.WriteString(m.titleView())
	b.WriteByte('\n')
	b.WriteString(m.pillsView())
	b.WriteByte('\n')
	b.WriteString(m.searchView())
	b.WriteByte('\n')
	b.WriteString(m.list.View())
	b.WriteByte('\n')
	b.WriteString(m.footerView())
	return b.String()
}

func (m model) titleView() string {
	title := titleStyle.Render("markroom")
	var total, unread int
	for _, rt := range m.cfg.Roots {
		c := m.stats[rt.Name]
		total += c.Total
		unread += c.Unread
	}
	meta := fmt.Sprintf(" %d docs · %d unread", total, unread)
	if m.scanning {
		meta += " · " + m.spin.View() + " rescanning…"
	}
	return title + titleMetaStyle.Render(meta)
}

// pillsView renders the root filter bar: every configured root in config
// order, zero-count roots included — same as the web inbox.
func (m model) pillsView() string {
	pill := func(label string, unread int, active bool) string {
		if unread > 0 {
			label += " " + unreadCntStyle.Render(fmt.Sprint(unread))
		}
		if active {
			return pillActiveStyle.Render(label)
		}
		return pillStyle.Render(label)
	}
	var totalUnread int
	for _, rt := range m.cfg.Roots {
		totalUnread += m.stats[rt.Name].Unread
	}
	parts := []string{pill("all", totalUnread, m.rootIdx == 0)}
	for i, rt := range m.cfg.Roots {
		label := rt.Name
		if rt.IsMirror() {
			label = "↗" + label // held here, written on another machine
		}
		parts = append(parts, pill(label, m.stats[rt.Name].Unread, m.rootIdx == i+1))
	}
	return lipgloss.JoinHorizontal(lipgloss.Center, parts...)
}

func (m model) searchView() string {
	if m.searching {
		return m.search.View()
	}
	if m.query != "" {
		return fmt.Sprintf("search: %q — %d match(es)", m.query, len(m.list.Items()))
	}
	return hintStyle.Render("press / to search")
}

func (m model) footerView() string {
	if m.status != "" {
		return errStyle.Render(m.status)
	}
	hints := "enter open · / search · tab root · r rescan"
	if len(m.cfg.Peers) > 0 {
		hints += " · s sync"
	}
	return hintStyle.Render(hints + " · q quit")
}
