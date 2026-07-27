package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jobs-build/jobs-iroh/api"
)

// statsModel is the server-stats view: store disk usage, ref count,
// request/node counters and uptime, refreshed by the poll tick.
type statsModel struct {
	c      *client
	stats  api.StatsReply
	loaded bool
}

func newStatsModel(c *client) statsModel { return statsModel{c: c} }

func (m statsModel) captures() bool { return false }

func (m statsModel) update(msg tea.Msg) (statsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tickMsg:
		return m, fetchStatsCmd(m.c)
	case statsMsg:
		m.stats = msg.stats
		m.loaded = true
	case tea.KeyMsg:
		if msg.String() == "r" {
			return m, fetchStatsCmd(m.c)
		}
	}
	return m, nil
}

func (m statsModel) view() string {
	if !m.loaded {
		return styleFaint.Render("loading…")
	}
	return strings.Join(statsLines(m.stats), "\n")
}
