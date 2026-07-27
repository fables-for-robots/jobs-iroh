package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jobs-build/jobs-iroh/api"
)

// refsModel is the ref browser: a prefix-filterable listing, one page of up
// to refsLimit refs, cursor-scrolled. '/' edits the prefix; enter applies
// it (and refetches), esc abandons the edit.
type refsModel struct {
	c             *client
	width, height int

	input  textinput.Model
	prefix string // applied filter (input.Value() is the draft)

	refs   []api.RefInfo
	loaded bool
	cursor int
	top    int
}

func newRefsModel(c *client) refsModel {
	in := textinput.New()
	in.Prompt = "prefix: "
	in.Placeholder = "e.g. build-output:"
	in.CharLimit = 256
	return refsModel{c: c, input: in}
}

// captures: while the prefix input is focused every key belongs to it.
func (m refsModel) captures() bool { return m.input.Focused() }

func (m refsModel) setSize(w, h int) refsModel {
	m.width, m.height = w, h
	m.input.Width = max(16, w-12)
	return m
}

func (m refsModel) update(msg tea.Msg) (refsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, fetchRefsCmd(m.c, m.prefix)
	case refsMsg:
		if msg.prefix != m.prefix {
			return m, nil // stale fetch from before an edit
		}
		m.refs = msg.refs
		m.loaded = true
		m.cursor = clamp(m.cursor, len(m.refs))
		m.top = window(len(m.refs), m.rowsHeight(), m.cursor, m.top)
		return m, nil

	case tea.KeyMsg:
		if m.input.Focused() {
			switch msg.String() {
			case "enter":
				m.input.Blur()
				m.prefix = m.input.Value()
				m.cursor, m.top = 0, 0
				m.loaded = false
				return m, fetchRefsCmd(m.c, m.prefix)
			case "esc":
				m.input.Blur()
				m.input.SetValue(m.prefix)
				return m, nil
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		n := len(m.refs)
		switch msg.String() {
		case "/":
			return m, m.input.Focus()
		case "up", "k":
			m.cursor = clamp(m.cursor-1, n)
		case "down", "j":
			m.cursor = clamp(m.cursor+1, n)
		case "pgup":
			m.cursor = clamp(m.cursor-m.rowsHeight(), n)
		case "pgdown":
			m.cursor = clamp(m.cursor+m.rowsHeight(), n)
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = clamp(n-1, n)
		case "r":
			return m, fetchRefsCmd(m.c, m.prefix)
		}
		m.top = window(n, m.rowsHeight(), m.cursor, m.top)
		return m, nil
	}
	return m, nil
}

// rowsHeight leaves room for the filter line + table header.
func (m refsModel) rowsHeight() int { return max(1, m.height-2) }

// refWidths widens the name column into the pane's slack.
func (m refsModel) refWidths() []int {
	w := make([]int, len(refCols))
	copy(w, refCols)
	if slack := m.width - (refCols[0] + refCols[1] + refCols[2] + 4); slack > 0 {
		w[0] += slack
	}
	return w
}

func (m refsModel) view() string {
	var b strings.Builder
	filter := m.input.View()
	if !m.input.Focused() && m.input.Value() == "" {
		filter = styleFaint.Render("prefix: (all — press / to filter)")
	}
	count := fmt.Sprintf("  %d refs", len(m.refs))
	if len(m.refs) == refsLimit {
		count = fmt.Sprintf("  first %d refs", refsLimit)
	}
	b.WriteString(filter + styleFaint.Render(count))
	b.WriteByte('\n')

	widths := m.refWidths()
	b.WriteString(styleHeader.Render(joinCells(refHeader, widths)))
	b.WriteByte('\n')
	if len(m.refs) == 0 {
		if m.loaded {
			b.WriteString(styleFaint.Render("no refs match"))
		} else {
			b.WriteString(styleFaint.Render("loading…"))
		}
		return b.String()
	}
	now := time.Now()
	end := min(m.top+m.rowsHeight(), len(m.refs))
	for i := m.top; i < end; i++ {
		line := joinCells(refRow(m.refs[i], now), widths)
		if i == m.cursor {
			line = styleSelected.Render(line)
		}
		b.WriteString(line)
		if i != end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
