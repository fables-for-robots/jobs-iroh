package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jobs-build/jobs-iroh/wire"
)

// fleetModel is the runner-fleet view: a read-only table refreshed by the
// poll tick, scrollable when the fleet outgrows the pane.
type fleetModel struct {
	c             *client
	width, height int
	runners       []wire.RunnerInfo
	loaded        bool
	top           int
}

func newFleetModel(c *client) fleetModel { return fleetModel{c: c} }

func (m fleetModel) captures() bool { return false }

func (m fleetModel) setSize(w, h int) fleetModel {
	m.width, m.height = w, h
	return m
}

func (m fleetModel) update(msg tea.Msg) (fleetModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, fetchFleetCmd(m.c)
	case fleetMsg:
		m.runners = msg.runners
		m.loaded = true
		m.top = window(len(m.runners), m.rowsHeight(), m.top, m.top)
	case tea.KeyMsg:
		switch msg.String() {
		case "up", "k":
			m.top = max(0, m.top-1)
		case "down", "j":
			m.top = min(max(0, len(m.runners)-m.rowsHeight()), m.top+1)
		case "r":
			return m, fetchFleetCmd(m.c)
		}
	}
	return m, nil
}

func (m fleetModel) rowsHeight() int { return max(1, m.height-1) }

func (m fleetModel) view() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(joinCells(fleetHeader, fleetCols)))
	b.WriteByte('\n')
	if len(m.runners) == 0 {
		if m.loaded {
			b.WriteString(styleFaint.Render("no runners connected"))
		} else {
			b.WriteString(styleFaint.Render("loading…"))
		}
		return b.String()
	}
	now := time.Now()
	end := min(m.top+m.rowsHeight(), len(m.runners))
	for i := m.top; i < end; i++ {
		b.WriteString(joinCells(runnerRow(m.runners[i], now), fleetCols))
		if i != end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
