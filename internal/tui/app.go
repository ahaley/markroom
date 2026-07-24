// Package tui implements the terminal reading UI: the same inbox → document
// flow the web UI serves, rendered with Bubble Tea and Glamour.
package tui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/ahaley/markroom/internal/config"
	"github.com/ahaley/markroom/internal/index"
)

// Run starts the TUI and blocks until the user quits or ctx is canceled.
// initialRoot, when non-empty, starts the inbox filtered to that root.
func Run(ctx context.Context, store *index.Store, cfg *config.Config, initialRoot string) error {
	// Detect the terminal background once, before Bubble Tea owns the tty;
	// querying it mid-program would race the input loop.
	mdStyle := "light"
	if lipgloss.HasDarkBackground() {
		mdStyle = "dark"
	}

	m := newModel(ctx, store, cfg, initialRoot, mdStyle)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		if ctx.Err() != nil {
			return nil // canceled by signal: a clean exit
		}
		return err
	}
	return nil
}

type screen int

const (
	screenInbox screen = iota
	screenDoc
)

// rescanEvery matches the serve daemon's cadence; the TUI tick only re-reads
// the index (scanning stays on the r key or a concurrently running daemon).
const refreshEvery = 30 * time.Second

type model struct {
	ctx   context.Context
	store *index.Store
	cfg   *config.Config

	screen screen
	width  int
	height int
	status string // transient error line, cleared on the next good refresh

	// inbox
	list      list.Model
	search    textinput.Model
	searching bool   // search input focused
	query     string // active FTS query, "" = plain listing
	rootIdx   int    // 0 = all roots, i>0 = cfg.Roots[i-1]
	stats     map[string]index.RootCount
	scanning  bool
	spin      spinner.Model

	// doc
	doc     *index.Doc
	vp      viewport.Model
	rawBody string // frontmatter-stripped markdown, kept for re-wrap on resize
	mdStyle string // glamour standard style: "dark" or "light"
}

func newModel(ctx context.Context, store *index.Store, cfg *config.Config, initialRoot, mdStyle string) model {
	d := list.NewDefaultDelegate()
	l := list.New(nil, d, 0, 0)
	l.SetShowTitle(false)
	l.SetShowStatusBar(false)
	l.SetShowHelp(false)
	l.SetFilteringEnabled(false) // "/" runs FTS search instead
	l.DisableQuitKeybindings()

	in := textinput.New()
	in.Placeholder = "search titles and bodies…"
	in.Prompt = "/ "
	in.CharLimit = 200

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))

	rootIdx := 0
	for i, r := range cfg.Roots {
		if strings.EqualFold(r.Name, initialRoot) {
			rootIdx = i + 1
			break
		}
	}

	return model{
		ctx:     ctx,
		store:   store,
		cfg:     cfg,
		list:    l,
		search:  in,
		rootIdx: rootIdx,
		stats:   map[string]index.RootCount{},
		spin:    sp,
		mdStyle: mdStyle,
	}
}

// activeRoot returns the name of the root filter, or "" for all roots.
func (m model) activeRoot() string {
	if m.rootIdx == 0 || m.rootIdx > len(m.cfg.Roots) {
		return ""
	}
	return m.cfg.Roots[m.rootIdx-1].Name
}

func (m model) Init() tea.Cmd {
	return tea.Batch(m.refreshCmd(), m.spin.Tick, tickCmd())
}

// --- messages ---

type tickMsg time.Time

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type docsMsg struct {
	docs  []index.Doc
	stats map[string]index.RootCount
	err   error
}

type scanDoneMsg struct {
	cfg *config.Config
	err error
}

type openedMsg struct {
	doc  *index.Doc
	body string
	err  error
}

type unreadDoneMsg struct{ err error }

// --- commands ---

// refreshCmd re-reads the current inbox query from the index.
func (m model) refreshCmd() tea.Cmd {
	ctx, store := m.ctx, m.store
	query, root := m.query, m.activeRoot()
	return func() tea.Msg {
		var docs []index.Doc
		var err error
		if query != "" {
			docs, err = store.Search(ctx, query, root)
		} else {
			docs, err = store.ListDocs(ctx, root)
		}
		if err != nil {
			return docsMsg{err: err}
		}
		stats, err := store.AllRootStats(ctx)
		if err != nil {
			return docsMsg{err: err}
		}
		return docsMsg{docs: docs, stats: stats}
	}
}

// scanCmd mirrors the server's reloadAndScan: pick up config changes, then
// rescan every root. ScanAll logs (not returns) per-root failures.
func (m model) scanCmd() tea.Cmd {
	ctx, store := m.ctx, m.store
	return func() tea.Msg {
		cfg, err := config.Load()
		if err != nil {
			return scanDoneMsg{err: err}
		}
		store.ScanAll(ctx, cfg.Roots)
		return scanDoneMsg{cfg: cfg}
	}
}

// openCmd reads a document from disk and marks it read — opening is reading,
// exactly like the web UI's document handler.
func (m model) openCmd(d index.Doc) tea.Cmd {
	ctx, store, cfg := m.ctx, m.store, m.cfg
	return func() tea.Msg {
		root := cfg.Find(d.Root)
		if root == nil {
			return openedMsg{err: fmt.Errorf("root %q is no longer registered", d.Root)}
		}
		full := filepath.Join(root.Path, filepath.FromSlash(d.RelPath))
		content, err := os.ReadFile(full)
		if err != nil {
			return openedMsg{err: err}
		}
		// Index may lag the disk; upsert so what we show is what we record.
		if info, statErr := os.Stat(full); statErr == nil {
			_ = store.UpsertDoc(ctx, root.Name, d.RelPath, content, info.ModTime(), info.Size())
		}
		doc, err := store.GetDoc(ctx, root.Name, d.RelPath)
		if err != nil {
			return openedMsg{err: err}
		}
		sum := sha256.Sum256(content)
		_ = store.MarkRead(ctx, doc.ID, hex.EncodeToString(sum[:]))
		return openedMsg{doc: doc, body: index.StripFrontmatter(string(content))}
	}
}

func (m model) unreadCmd(docID int64) tea.Cmd {
	ctx, store := m.ctx, m.store
	return func() tea.Msg {
		return unreadDoneMsg{err: store.MarkUnread(ctx, docID)}
	}
}

// --- update ---

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.list.SetSize(msg.Width, max(msg.Height-inboxChromeLines, 1))
		m.search.Width = max(msg.Width-4, 10)
		m.vp.Width = msg.Width
		m.vp.Height = max(msg.Height-docChromeLines, 1)
		if m.screen == screenDoc {
			m.vp.SetContent(m.renderMarkdown())
		}
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}

	case tickMsg:
		cmds := []tea.Cmd{tickCmd()}
		if m.screen == screenInbox && !m.scanning {
			cmds = append(cmds, m.refreshCmd())
		}
		return m, tea.Batch(cmds...)

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case docsMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		m.status = ""
		m.stats = msg.stats
		items := make([]list.Item, len(msg.docs))
		for i, d := range msg.docs {
			items[i] = docItem{doc: d}
		}
		return m, m.list.SetItems(items)

	case scanDoneMsg:
		m.scanning = false
		if msg.err != nil {
			m.status = "rescan failed: " + msg.err.Error()
			return m, nil
		}
		m.cfg = msg.cfg
		if m.rootIdx > len(m.cfg.Roots) {
			m.rootIdx = 0
		}
		return m, m.refreshCmd()

	case openedMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
			return m, nil
		}
		m.doc = msg.doc
		m.rawBody = msg.body
		m.screen = screenDoc
		m.vp = viewport.New(m.width, max(m.height-docChromeLines, 1))
		m.vp.SetContent(m.renderMarkdown())
		return m, nil

	case unreadDoneMsg:
		if msg.err != nil {
			m.status = "error: " + msg.err.Error()
		}
		m.screen = screenInbox
		return m, m.refreshCmd()
	}

	switch m.screen {
	case screenDoc:
		return m.updateDoc(msg)
	default:
		return m.updateInbox(msg)
	}
}

// --- view ---

func (m model) View() string {
	if m.width == 0 {
		return "loading…"
	}
	if m.screen == screenDoc {
		return m.viewDoc()
	}
	return m.viewInbox()
}
