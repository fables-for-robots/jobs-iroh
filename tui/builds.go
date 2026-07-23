package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/fables-for-robots/jobs-iroh/api"
)

// buildsMode is the builds view's sub-screen.
type buildsMode int

const (
	modeList buildsMode = iota
	modeDetail
	modeLogs
	modeConfirmCancel
	modeConfirmDelete
)

// buildsModel is the builds view: the polled request list, and per-request
// the pushed snapshot watch with node logs and cancel/delete.
type buildsModel struct {
	c             *client
	width, height int

	mode   buildsMode
	loaded bool

	// list
	rows   []api.RequestInfo
	cursor int
	top    int

	// detail: one live watch stream per entered request
	reqID        string
	snap         api.Snapshot
	snapAt       time.Time
	haveSnap     bool
	dcursor      int
	dtop         int
	watchSeq     int // guards stale stream messages
	watchCh      <-chan watchEvent
	watchCancel  func()
	watchErr     error // stream down; tick retries transport failures
	watchPending bool  // a start cmd is in flight — don't double-start

	// logs
	logNode    string
	logSeq     int
	logCh      <-chan logEvent
	logCancel  func()
	logErr     error
	logPending bool
	logBuf     string
	logGen     uint64
	follow     bool
	vp         viewport.Model
}

func newBuildsModel(c *client) buildsModel {
	return buildsModel{c: c, vp: viewport.New(80, 20), follow: true}
}

// captures reports whether the view owns the keyboard (any sub-screen of
// the list: tab switching would leak streams and hide state changes).
func (m buildsModel) captures() bool { return m.mode != modeList }

func (m buildsModel) setSize(w, h int) buildsModel {
	m.width, m.height = w, h
	m.vp.Width = w
	m.vp.Height = max(1, h-2)
	return m
}

func (m buildsModel) update(msg tea.Msg) (buildsModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.updateKey(msg)

	case tickMsg:
		// The tick is the poll (list) and the stream retry loop (detail/
		// logs). Server-answered errors (not-found etc.) are terminal —
		// only transport failures retry.
		if m.mode == modeList {
			return m, fetchRequestsCmd(m.c)
		}
		var cmds []tea.Cmd
		if m.watchErr != nil && !m.watchPending && !isServerError(m.watchErr) {
			m.watchSeq++
			m.watchPending = true
			cmds = append(cmds, startWatchCmd(m.c, m.watchSeq, m.reqID))
		}
		if m.mode == modeLogs && m.logErr != nil && !m.logPending && !isServerError(m.logErr) {
			m.logSeq++
			m.logPending = true
			cmds = append(cmds, startLogsCmd(m.c, m.logSeq, m.logNode))
		}
		return m, tea.Batch(cmds...)

	case requestsMsg:
		m.rows = msg.rows
		m.loaded = true
		m.cursor = clamp(m.cursor, len(m.rows))
		m.top = window(len(m.rows), m.listHeight(), m.cursor, m.top)
		return m, nil

	// --- watch stream ---
	case watchStartedMsg:
		if msg.seq != m.watchSeq || m.mode == modeList {
			msg.cancel()
			return m, nil
		}
		m.watchCh = msg.ch
		m.watchCancel = msg.cancel
		m.watchErr = nil
		m.watchPending = false
		return m, awaitWatchCmd(msg.seq, msg.ch)
	case watchFailedMsg:
		if msg.seq != m.watchSeq || m.mode == modeList {
			return m, nil
		}
		m.watchErr = msg.err
		m.watchPending = false
		return m, nil // tick retries transport failures
	case watchEventMsg:
		if msg.seq != m.watchSeq || m.mode == modeList {
			return m, nil
		}
		// Always re-arm the await: after an error event the reader closes
		// the channel, so the re-armed await delivers watchClosedMsg, which
		// clears the stream state.
		if msg.ev.err != nil {
			m.watchErr = msg.ev.err
			return m, awaitWatchCmd(msg.seq, m.watchCh)
		}
		m.snap = msg.ev.snap
		m.snapAt = time.Now()
		m.haveSnap = true
		m.fixDetailCursor()
		return m, awaitWatchCmd(msg.seq, m.watchCh)
	case watchClosedMsg:
		if msg.seq != m.watchSeq || m.mode == modeList {
			return m, nil
		}
		m.watchCh = nil
		m.watchCancel = nil
		if m.watchErr == nil && m.haveSnap && !m.snap.Terminal {
			// Closed without a terminal snapshot: server side went away.
			m.watchErr = fmt.Errorf("watch stream closed")
		}
		return m, nil

	// --- log stream ---
	case logStartedMsg:
		if msg.seq != m.logSeq || m.mode != modeLogs {
			msg.cancel()
			return m, nil
		}
		m.logCh = msg.ch
		m.logCancel = msg.cancel
		m.logErr = nil
		m.logPending = false
		m.logBuf = logInitial(msg.view)
		m.logGen = msg.view.Gen
		m.vp.SetContent(m.logBuf)
		m.vp.GotoBottom()
		m.follow = true
		return m, awaitLogsCmd(msg.seq, msg.ch)
	case logFailedMsg:
		if msg.seq != m.logSeq || m.mode != modeLogs {
			return m, nil
		}
		m.logErr = msg.err
		m.logPending = false
		return m, nil
	case logEventMsg:
		if msg.seq != m.logSeq || m.mode != modeLogs {
			return m, nil
		}
		// Always re-arm the await (see watchEventMsg).
		if msg.ev.err != nil {
			m.logErr = msg.ev.err
			return m, awaitLogsCmd(msg.seq, m.logCh)
		}
		m.logBuf, m.logGen = logAppend(m.logBuf, m.logGen, msg.ev.chunk)
		m.vp.SetContent(m.logBuf)
		if m.follow {
			m.vp.GotoBottom()
		}
		return m, awaitLogsCmd(msg.seq, m.logCh)
	case logClosedMsg:
		if msg.seq == m.logSeq {
			m.logCh = nil
			m.logCancel = nil
		}
		return m, nil

	case actionDoneMsg:
		if msg.err != nil {
			return m, nil // root shows the error
		}
		if msg.op == "delete" && msg.requestID == m.reqID && m.mode != modeList {
			// The request is gone; unwind to the list.
			m = m.leaveDetail()
			return m, fetchRequestsCmd(m.c)
		}
		return m, nil
	}
	return m, nil
}

func (m buildsModel) updateKey(msg tea.KeyMsg) (buildsModel, tea.Cmd) {
	key := msg.String()
	switch m.mode {

	case modeList:
		n := len(m.rows)
		switch key {
		case "up", "k":
			m.cursor = clamp(m.cursor-1, n)
		case "down", "j":
			m.cursor = clamp(m.cursor+1, n)
		case "pgup":
			m.cursor = clamp(m.cursor-m.listHeight(), n)
		case "pgdown":
			m.cursor = clamp(m.cursor+m.listHeight(), n)
		case "g", "home":
			m.cursor = 0
		case "G", "end":
			m.cursor = clamp(n-1, n)
		case "r":
			return m, fetchRequestsCmd(m.c)
		case "enter":
			if m.cursor < n {
				return m.enterDetail(m.rows[m.cursor].RequestID)
			}
		}
		m.top = window(n, m.listHeight(), m.cursor, m.top)
		return m, nil

	case modeDetail:
		entries := m.entries()
		switch key {
		case "up", "k":
			m.dcursor = nextEntry(entries, m.dcursor, -1)
		case "down", "j":
			m.dcursor = nextEntry(entries, m.dcursor, +1)
		case "l", "enter":
			if node := selectedNode(entries, m.dcursor); node != "" {
				return m.enterLogs(node)
			}
		case "c":
			m.mode = modeConfirmCancel
		case "d":
			m.mode = modeConfirmDelete
		case "esc", "q":
			m = m.leaveDetail()
			return m, fetchRequestsCmd(m.c)
		}
		m.dtop = window(len(entries), m.detailHeight(), m.dcursor, m.dtop)
		return m, nil

	case modeLogs:
		switch key {
		case "esc", "q":
			return m.leaveLogs(), nil
		case "G", "end":
			m.vp.GotoBottom()
			m.follow = true
			return m, nil
		}
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		// Scrolling away pauses follow; returning to the bottom resumes it.
		m.follow = m.vp.AtBottom()
		return m, cmd

	case modeConfirmCancel:
		m.mode = modeDetail
		if key == "y" {
			return m, cancelRequestCmd(m.c, m.reqID)
		}
		return m, nil

	case modeConfirmDelete:
		m.mode = modeDetail
		if key == "y" {
			return m, deleteRequestCmd(m.c, m.reqID)
		}
		return m, nil
	}
	return m, nil
}

// --- transitions ---

func (m buildsModel) enterDetail(reqID string) (buildsModel, tea.Cmd) {
	m.mode = modeDetail
	m.reqID = reqID
	m.snap = api.Snapshot{}
	m.haveSnap = false
	m.watchCh = nil
	m.watchErr = nil
	m.watchPending = true
	m.dcursor, m.dtop = 0, 0
	m.watchSeq++
	return m, startWatchCmd(m.c, m.watchSeq, reqID)
}

func (m buildsModel) leaveDetail() buildsModel {
	if m.watchCancel != nil {
		m.watchCancel()
		m.watchCancel = nil
	}
	m = m.leaveLogs()
	m.mode = modeList
	m.watchSeq++ // orphan any in-flight stream messages
	m.watchCh = nil
	m.watchPending = false
	m.watchErr = nil
	return m
}

func (m buildsModel) enterLogs(node string) (buildsModel, tea.Cmd) {
	m.mode = modeLogs
	m.logNode = node
	m.logCh = nil
	m.logErr = nil
	m.logPending = true
	m.logBuf = ""
	m.logGen = 0
	m.vp.SetContent("")
	m.follow = true
	m.logSeq++
	return m, startLogsCmd(m.c, m.logSeq, node)
}

func (m buildsModel) leaveLogs() buildsModel {
	if m.logCancel != nil {
		m.logCancel()
		m.logCancel = nil
	}
	if m.mode == modeLogs {
		m.mode = modeDetail
	}
	m.logSeq++
	m.logCh = nil
	m.logPending = false
	m.logErr = nil
	return m
}

// --- geometry & derived state ---

func (m buildsModel) listHeight() int { return max(1, m.height-1) } // minus header row

// detailHeight reserves the two title lines, the blank separator, and the
// confirm-prompt line, so the prompt never falls off a full pane.
func (m buildsModel) detailHeight() int { return max(1, m.height-4) }

// entries derives the detail rows from the latest snapshot with elapsed
// times ticked forward since it arrived.
func (m buildsModel) entries() []detailEntry {
	if !m.haveSnap {
		return nil
	}
	return detailEntries(m.snap, time.Since(m.snapAt))
}

// fixDetailCursor re-anchors the cursor after a snapshot reshuffles groups.
func (m *buildsModel) fixDetailCursor() {
	entries := m.entries()
	if m.dcursor >= len(entries) || (len(entries) > 0 && entries[clamp(m.dcursor, len(entries))].header) {
		m.dcursor = firstEntry(entries)
		if m.dcursor < 0 {
			m.dcursor = 0
		}
	}
	m.dtop = window(len(entries), m.detailHeight(), m.dcursor, m.dtop)
}

// --- view ---

func (m buildsModel) view() string {
	switch m.mode {
	case modeList:
		return m.viewList()
	case modeLogs:
		return m.viewLogs()
	default: // detail + confirms render the detail screen
		return m.viewDetail()
	}
}

func (m buildsModel) viewList() string {
	var b strings.Builder
	b.WriteString(styleHeader.Render(joinCells(buildHeader, buildCols)))
	b.WriteByte('\n')
	if len(m.rows) == 0 {
		if m.loaded {
			b.WriteString(styleFaint.Render("no requests"))
		} else {
			b.WriteString(styleFaint.Render("loading…"))
		}
		return b.String()
	}
	now := time.Now()
	h := m.listHeight()
	end := min(m.top+h, len(m.rows))
	for i := m.top; i < end; i++ {
		cells := requestRow(m.rows[i], now)
		line := joinCells(cells, buildCols)
		if i == m.cursor {
			line = styleSelected.Render(line)
		} else {
			// Re-render with only the phase cell colored.
			cells[2] = phaseStyle(m.rows[i].Phase).Render(padCell(cells[2], buildCols[2]))
			line = recompose(cells, buildCols)
		}
		b.WriteString(line)
		if i != end-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// recompose joins cells where some are pre-styled (already padded).
func recompose(cells []string, widths []int) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		if lipglossWidth(c) == widths[i] {
			parts[i] = c // already padded (possibly styled)
		} else {
			parts[i] = padCell(c, widths[i])
		}
	}
	return strings.Join(parts, "  ")
}

func (m buildsModel) viewDetail() string {
	var b strings.Builder
	title := fmt.Sprintf("request %s", m.reqID)
	b.WriteString(styleTitle.Render(title))
	b.WriteByte('\n')
	if !m.haveSnap {
		if m.watchErr != nil {
			b.WriteString(styleError.Render("watch: " + m.watchErr.Error()))
		} else {
			b.WriteString(styleFaint.Render("waiting for snapshot…"))
		}
		b.WriteString("\n\n")
		b.WriteString(m.confirmLine())
		return b.String()
	}
	status := fmt.Sprintf("%s  %s", m.snap.Phase, fmtCounts(m.snap.Counts))
	b.WriteString(phaseStyle(m.snap.Phase).Render(m.snap.Phase))
	b.WriteString(styleFaint.Render(status[len(m.snap.Phase):]))
	if m.watchErr != nil {
		b.WriteString("  " + styleError.Render("stream lost, retrying…"))
	} else if m.snap.Terminal {
		b.WriteString(styleFaint.Render("  (final)"))
	}
	b.WriteString("\n\n")

	entries := m.entries()
	h := m.detailHeight()
	top := window(len(entries), h, m.dcursor, m.dtop)
	end := min(top+h, len(entries))
	for i := top; i < end; i++ {
		e := entries[i]
		var line string
		switch {
		case e.header:
			line = phaseStyle(e.phase).Render(e.text)
		case i == m.dcursor:
			line = styleSelected.Render(padCell("  "+e.text, m.width))
		default:
			line = "  " + e.text
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString(m.confirmLine())
	return b.String()
}

// confirmLine renders the pending confirm prompt (empty otherwise).
func (m buildsModel) confirmLine() string {
	switch m.mode {
	case modeConfirmCancel:
		return styleError.Render(fmt.Sprintf("cancel request %s? y/N", m.reqID))
	case modeConfirmDelete:
		return styleError.Render(fmt.Sprintf("delete request %s? y/N", m.reqID))
	}
	return ""
}

func (m buildsModel) viewLogs() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("logs ") + shortNode(m.logNode))
	if m.follow {
		b.WriteString("  " + styleFaint.Render("following"))
	}
	if m.logErr != nil {
		b.WriteString("  " + styleError.Render(m.logErr.Error()))
	}
	b.WriteByte('\n')
	b.WriteString(m.vp.View())
	return b.String()
}
