package tui

import (
	"context"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/ahaley/markroom/internal/config"
)

func testModel(t *testing.T, initialRoot string) model {
	t.Helper()
	cfg := &config.Config{Roots: []config.Root{
		{Name: "alpha", Path: "/tmp/alpha"},
		{Name: "beta", Path: "/tmp/beta"},
	}}
	// store stays nil: these tests exercise pure Update logic and never run
	// the returned tea.Cmds that would touch the database.
	return newModel(context.Background(), nil, cfg, initialRoot, "dark")
}

func press(t *testing.T, m model, key tea.KeyMsg) model {
	t.Helper()
	next, _ := m.Update(key)
	got, ok := next.(model)
	if !ok {
		t.Fatalf("Update returned %T, want model", next)
	}
	return got
}

func runes(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestInitialRootMatchesCaseInsensitively(t *testing.T) {
	if got := testModel(t, "BETA").rootIdx; got != 2 {
		t.Errorf("rootIdx = %d, want 2", got)
	}
	if got := testModel(t, "nope").rootIdx; got != 0 {
		t.Errorf("unknown root: rootIdx = %d, want 0", got)
	}
}

func TestRootCyclingWraps(t *testing.T) {
	m := testModel(t, "")
	want := []struct {
		idx  int
		root string
	}{{1, "alpha"}, {2, "beta"}, {0, ""}}
	for _, w := range want {
		m = press(t, m, tea.KeyMsg{Type: tea.KeyTab})
		if m.rootIdx != w.idx || m.activeRoot() != w.root {
			t.Fatalf("after tab: rootIdx=%d activeRoot=%q, want %d %q", m.rootIdx, m.activeRoot(), w.idx, w.root)
		}
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.rootIdx != 2 {
		t.Errorf("after shift+tab from all: rootIdx = %d, want 2", m.rootIdx)
	}
}

func TestSearchStateTransitions(t *testing.T) {
	m := testModel(t, "")

	m = press(t, m, runes("/"))
	if !m.searching {
		t.Fatal("`/` should focus the search input")
	}

	// Typed characters go to the input, not the key bindings ('q' must not quit).
	m = press(t, m, runes("q"))
	if !m.searching || m.search.Value() != "q" {
		t.Fatalf("searching=%v value=%q, want typing captured by input", m.searching, m.search.Value())
	}

	m = press(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	if m.searching || m.query != "q" {
		t.Fatalf("after enter: searching=%v query=%q, want committed query", m.searching, m.query)
	}

	// esc with an active query clears it back to the full inbox.
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.query != "" || m.search.Value() != "" {
		t.Fatalf("after esc: query=%q input=%q, want both cleared", m.query, m.search.Value())
	}
}

func TestEscWhileTypingKeepsCommittedQuery(t *testing.T) {
	m := testModel(t, "")
	m.query = "kept"
	m.search.SetValue("kept")

	m = press(t, m, runes("/"))
	m = press(t, m, runes("x"))
	m = press(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.searching || m.query != "kept" || m.search.Value() != "kept" {
		t.Fatalf("after esc mid-edit: searching=%v query=%q input=%q, want committed query restored",
			m.searching, m.query, m.search.Value())
	}
}
