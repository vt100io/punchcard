package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// `punchcard tui` is a terminal UI for a Punchcard server on this machine. It
// talks to the server over the control socket (control.go); to watch a
// remote server, SSH to it and run `punchcard tui` there.

func tuiCmd() error {
	sock := Config{StateDir: stateDir()}.SocketPath()
	_, err := tea.NewProgram(newTUI(newControlClient(sock))).Run()
	return err
}

// --- control socket client ---

type controlClient struct {
	http *http.Client
	sock string
}

func newControlClient(sock string) *controlClient {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}
	return &controlClient{http: &http.Client{Transport: tr}, sock: sock}
}

func (c *controlClient) get(path string) (*http.Response, error) {
	resp, err := c.http.Get("http://punchcard" + path)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	return resp, nil
}

// --- messages ---

type (
	connectedMsg    struct{ events <-chan Event }
	disconnectedMsg struct{ err error }
	reconnectMsg    struct{}
	stateMsg        Snapshot
	eventMsg        Event
	logMsg          struct {
		id    string
		lines []string
	}
	tickMsg time.Time
)

// connect subscribes to /events. Subscribing before reading /state means no
// change can slip between the two.
func connect(c *controlClient) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.get("/events")
		if err != nil {
			return disconnectedMsg{err}
		}
		ch := make(chan Event, 256)
		go func() {
			defer resp.Body.Close()
			defer close(ch)
			sc := bufio.NewScanner(resp.Body)
			sc.Buffer(make([]byte, 64*1024), 1024*1024)
			for sc.Scan() {
				var e Event
				if json.Unmarshal(sc.Bytes(), &e) == nil {
					ch <- e
				}
			}
		}()
		return connectedMsg{ch}
	}
}

func waitForEvent(ch <-chan Event) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return disconnectedMsg{fmt.Errorf("server closed the connection")}
		}
		return eventMsg(e)
	}
}

func fetchState(c *controlClient) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.get("/state")
		if err != nil {
			return disconnectedMsg{err}
		}
		defer resp.Body.Close()
		var s Snapshot
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			return disconnectedMsg{err}
		}
		return stateMsg(s)
	}
}

func fetchLog(c *controlClient, id string) tea.Cmd {
	return func() tea.Msg {
		resp, err := c.get("/builds/" + id)
		if err != nil {
			return logMsg{id: id, lines: []string{"(log not available: " + err.Error() + ")"}}
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		// Only complete lines: a partial last line arrives later as an event.
		var lines []string
		if i := strings.LastIndexByte(string(data), '\n'); i >= 0 {
			lines = strings.Split(string(data[:i]), "\n")
		}
		return logMsg{id: id, lines: lines}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// --- model ---

// repoRow is one repo in the Repos pane: its running or latest build.
type repoRow struct {
	name    string
	build   Build
	running bool
}

type tui struct {
	client    *controlClient
	host      string
	connected bool
	events    <-chan Event
	err       error
	state     Snapshot
	repos     []repoRow
	selected  string // repo name, so the selection survives reordering
	now       time.Time

	// The log pane shows one build's log.
	logID  string
	log    []string
	offset int  // first visible line
	follow bool // keep the newest line in view
	width  int
	height int
}

func newTUI(c *controlClient) *tui {
	host, _ := os.Hostname()
	return &tui{client: c, host: host, follow: true, now: time.Now()}
}

func (m *tui) Init() tea.Cmd { return tea.Batch(connect(m.client), tick()) }

func (m *tui) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.clampScroll()

	case tea.KeyPressMsg:
		return m, m.key(msg.String())

	case tickMsg:
		m.now = time.Time(msg)
		return m, tick()

	case connectedMsg:
		m.connected, m.err, m.events = true, nil, msg.events
		return m, tea.Batch(fetchState(m.client), waitForEvent(m.events))

	case disconnectedMsg:
		m.connected, m.err = false, msg.err
		return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg { return reconnectMsg{} })

	case reconnectMsg:
		return m, connect(m.client)

	case stateMsg:
		m.state = Snapshot(msg)
		m.repos = repoRows(m.state)
		if m.selected == "" && len(m.repos) > 0 {
			m.selected = m.repos[0].name
		}
		return m, m.showSelected()

	case eventMsg:
		e := Event(msg)
		cmds := []tea.Cmd{waitForEvent(m.events)} // keep reading the stream
		if e.Kind == eventLine {
			if e.Build.ID == m.logID {
				switch {
				case e.N == len(m.log): // the next line
					m.log = append(m.log, e.Line)
					m.clampScroll()
				case e.N > len(m.log): // missed some: read the file again
					cmds = append(cmds, fetchLog(m.client, m.logID))
				} // e.N < len(m.log): already have it from the file
			}
		} else {
			// Anything else changes the queue or a result: re-read the whole
			// state, which also covers events this client missed.
			cmds = append(cmds, fetchState(m.client))
			if e.Kind == eventFinished && e.Build.ID == m.logID {
				cmds = append(cmds, fetchLog(m.client, m.logID))
			}
		}
		return m, tea.Batch(cmds...)

	case logMsg:
		// Events may have added lines while the file was being read.
		if msg.id == m.logID && len(msg.lines) >= len(m.log) {
			m.log = msg.lines
			m.clampScroll()
		}
	}
	return m, nil
}

func (m *tui) key(k string) tea.Cmd {
	switch k {
	case "q", "ctrl+c":
		return tea.Quit
	case "up", "k":
		m.moveSelection(-1)
		return m.showSelected()
	case "down", "j":
		m.moveSelection(1)
		return m.showSelected()
	case "pgup", "ctrl+u":
		m.scroll(-m.logHeight() / 2)
	case "pgdown", "ctrl+d":
		m.scroll(m.logHeight() / 2)
	case "g", "home":
		m.follow, m.offset = false, 0
	case "G", "end":
		m.follow = true
		m.clampScroll()
	}
	return nil
}

func (m *tui) moveSelection(d int) {
	for i, r := range m.repos {
		if r.name == m.selected {
			i = max(0, min(len(m.repos)-1, i+d))
			m.selected = m.repos[i].name
			return
		}
	}
}

// showSelected loads the log of the selected repo's running or latest build.
func (m *tui) showSelected() tea.Cmd {
	id := ""
	for _, r := range m.repos {
		if r.name == m.selected {
			id = r.build.ID
		}
	}
	if id == m.logID {
		return nil
	}
	m.logID, m.log, m.follow, m.offset = id, nil, true, 0
	if id == "" {
		return nil
	}
	return fetchLog(m.client, id)
}

func (m *tui) scroll(n int) {
	m.offset += n
	m.follow = false
	m.clampScroll()
	if m.offset >= len(m.log)-m.logHeight() {
		m.follow = true
	}
}

func (m *tui) clampScroll() {
	last := max(0, len(m.log)-m.logHeight())
	if m.follow {
		m.offset = last
	}
	m.offset = max(0, min(m.offset, last))
}

// repoRows lists each repo once, with its running or most recent build:
// the running repo first, then by most recent activity.
func repoRows(s Snapshot) []repoRow {
	seen := map[string]bool{}
	var rows []repoRow
	if s.Running != nil {
		rows = append(rows, repoRow{name: s.Running.Repo, build: *s.Running, running: true})
		seen[s.Running.Repo] = true
	}
	for _, b := range s.Recent { // newest first
		if seen[b.Repo] || b.Result == resultDropped {
			continue
		}
		seen[b.Repo] = true
		rows = append(rows, repoRow{name: b.Repo, build: b})
	}
	// Repos only in the queue so far.
	for _, b := range s.Queue {
		if !seen[b.Repo] {
			seen[b.Repo] = true
			rows = append(rows, repoRow{name: b.Repo, build: b})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].running && !rows[j].running })
	return rows
}

// --- view ---

var (
	cText   = lipgloss.Color("#c9ccd3")
	cDim    = lipgloss.Color("#8a919e")
	cLine   = lipgloss.Color("#333a46")
	cOK     = lipgloss.Color("#7cc28a")
	cBad    = lipgloss.Color("#ef7f77")
	cRun    = lipgloss.Color("#e3b964")
	cAmber  = lipgloss.Color("#e0a458")
	cSelBg  = lipgloss.Color("#262c37")
	cKeyFg  = lipgloss.Color("#14161b")
	cPrefix = []lipgloss.Style{fg(lipgloss.Color("#7aa7e6")), fg(lipgloss.Color("#c49bdc")), fg(lipgloss.Color("#6fc4c4")), fg(cAmber)}
)

func fg(c interface{ RGBA() (r, g, b, a uint32) }) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(c)
}

const leftMin, leftMax = 34, 60

func (m *tui) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "punchcard"
	return v
}

func (m *tui) render() string {
	if m.width == 0 || m.height == 0 {
		return ""
	}
	bodyH := max(6, m.height-2)
	leftW := max(leftMin, min(leftMax, m.width*2/5))
	rightW := max(20, m.width-leftW-1)

	queue := m.queueLines()
	queueH := max(4, min(len(queue)+2, bodyH/3))
	left := m.pane("[1] Repos", m.repoLines(leftW-4), leftW, bodyH-queueH, true) + "\n" +
		m.pane("[2] Queue", queue, leftW, queueH, false)
	right := m.pane(m.logTitle(), m.logLines(rightW-4), rightW, bodyH, false)

	body := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)
	return m.header() + "\n" + body + "\n" + m.footer()
}

// logHeight is the number of log lines that fit in the log pane.
func (m *tui) logHeight() int { return max(1, max(6, m.height-2)-2) }

func (m *tui) header() string {
	left := fg(cAmber).Bold(true).Render("▌punchcard") + "  "
	if m.connected {
		left += fg(cOK).Render("●") + " " + fg(cText).Render(m.host)
	} else {
		reason := "not connected"
		if m.err != nil && strings.Contains(m.err.Error(), "no such file") || m.err != nil && strings.Contains(m.err.Error(), "connection refused") {
			reason = "server not running — start it with: mise run serve"
		}
		left += fg(cBad).Render("○ "+reason) + fg(cDim).Render(" · retrying")
	}

	worker := fg(cOK).Render("●") + " worker idle"
	if m.state.Running != nil {
		worker = fg(cRun).Render("●") + " worker busy"
	}
	queued := "queue empty"
	if n := len(m.state.Queue); n > 0 {
		queued = fmt.Sprintf("%d queued", n)
	}
	right := worker + fg(cDim).Render(" · ") + queued + fg(cDim).Render(" · ") + m.now.Format("15:04")

	gap := max(1, m.width-lipgloss.Width(left)-lipgloss.Width(right))
	return left + strings.Repeat(" ", gap) + right
}

func (m *tui) footer() string {
	keys := [][2]string{{"↑↓", "select"}, {"pgup/pgdn", "scroll log"}, {"g/G", "top/follow"}, {"q", "quit"}}
	var parts []string
	for _, k := range keys {
		parts = append(parts, lipgloss.NewStyle().Foreground(cKeyFg).Background(cAmber).Bold(true).Render(" "+k[0]+" ")+" "+k[1])
	}
	return ansi.Truncate(strings.Join(parts, "  "), m.width, "")
}

func (m *tui) repoLines(w int) []string {
	if len(m.repos) == 0 {
		return []string{fg(cDim).Render("no builds yet")}
	}
	const stateW, ageW = 11, 4
	nameW := max(4, w-2-stateW-ageW-1)
	var lines []string
	for _, r := range m.repos {
		glyph, state, c := resultLook(r.build, r.running)
		age := ""
		if !r.running {
			age = ago(m.now, buildTime(r.build))
		}
		sel := r.name == m.selected
		st := func(c interface{ RGBA() (r, g, b, a uint32) }) lipgloss.Style {
			s := fg(c)
			if sel {
				s = s.Background(cSelBg)
			}
			return s
		}
		line := st(c).Render(glyph+" ") +
			st(cText).Render(padRight(ansi.Truncate(r.name, nameW, "…"), nameW)) +
			st(c).Render(padRight(state, stateW)) +
			st(cDim).Render(padLeft(age, ageW)+" ")
		lines = append(lines, line)
	}
	return lines
}

func (m *tui) queueLines() []string {
	var lines []string
	if r := m.state.Running; r != nil {
		lines = append(lines, fg(cRun).Render("● ")+r.Repo+" "+fg(cDim).Render(r.Trigger())+" "+shortSHA(r.SHA)+"  "+
			fg(cRun).Render(clock(m.now.Sub(r.Started))))
	}
	for i, b := range m.state.Queue {
		next := ""
		if i == 0 {
			next = fg(cDim).Render("  next")
		}
		lines = append(lines, fg(cDim).Render("○ ")+b.Repo+" "+fg(cDim).Render(b.Trigger())+" "+shortSHA(b.SHA)+next)
	}
	// Builds dropped in the last hour, so a vanished commit is explained.
	for _, b := range m.state.Recent {
		if b.Result == resultDropped && m.now.Sub(b.Finished) < time.Hour {
			// b.Error is "superseded by <sha>".
			lines = append(lines, fg(cDim).Render("↷ "+b.Repo+" "+shortSHA(b.SHA)+" replaced by "+strings.TrimPrefix(b.Error, "superseded by ")))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, fg(cDim).Render("nothing queued"))
	}
	return lines
}

func (m *tui) logTitle() string {
	t := "[3] Log"
	for _, r := range m.repos {
		if r.name != m.selected {
			continue
		}
		b := r.build
		_, state, c := resultLook(b, r.running)
		switch {
		case r.running:
			state = "running " + clock(m.now.Sub(b.Started))
		case b.Result == resultPassed:
			state = "passed in " + b.Finished.Sub(b.Started).Round(100*time.Millisecond).String()
		case b.Result == resultFailed || b.Result == resultError:
			state = b.Result + " after " + b.Finished.Sub(b.Started).Round(100*time.Millisecond).String()
		}
		// The state comes first: in a narrow terminal the end is cut off.
		dot := fg(cDim).Render(" · ")
		t += dot + fg(c).Render(state) + dot + fg(cText).Render(b.Repo) + dot + b.Trigger() + dot + shortSHA(b.SHA)
	}
	return t
}

func (m *tui) logLines(w int) []string {
	if m.logID == "" {
		return []string{fg(cDim).Render("no log for this build")}
	}
	start := min(m.offset, len(m.log))
	end := min(len(m.log), start+m.logHeight())
	var lines []string
	for _, l := range m.log[start:end] {
		lines = append(lines, styleLogLine(ansi.Truncate(l, w, "…")))
	}
	return lines
}

// styleLogLine colours a build log line: header keys, [task] prefixes,
// commands, and the verdict.
func styleLogLine(l string) string {
	for _, v := range []struct {
		word string
		c    interface{ RGBA() (r, g, b, a uint32) }
	}{{"PASSED", cOK}, {"FAILED", cBad}, {"ERROR", cBad}, {"SKIPPED", cDim}} {
		if strings.HasPrefix(l, v.word) {
			return fg(v.c).Bold(true).Render(v.word) + fg(cDim).Render(l[len(v.word):])
		}
	}
	for _, k := range []string{"repo:", "event:", "branch:", "commit:", "task:", "started:"} {
		if strings.HasPrefix(l, k) {
			return fg(cDim).Render(l[:len(k)]) + fg(cText).Render(l[len(k):])
		}
	}
	if strings.HasPrefix(l, "[") {
		if i := strings.Index(l, "] "); i > 0 {
			h := fnv.New32a()
			h.Write([]byte(l[1:i]))
			prefix := cPrefix[h.Sum32()%uint32(len(cPrefix))].Render(l[:i+2])
			return prefix + styleOutput(l[i+2:])
		}
	}
	return styleOutput(l)
}

// styleOutput colours one line of task output: commands dim, and the
// verdicts of common test runners (Go's ok/FAIL/--- FAIL) green or red.
func styleOutput(s string) string {
	switch {
	case strings.HasPrefix(s, "$ "):
		return fg(cDim).Render(s)
	case strings.HasPrefix(s, "FAIL") || strings.HasPrefix(s, "--- FAIL") || strings.HasPrefix(s, "ERROR"):
		return fg(cBad).Render(s)
	case strings.HasPrefix(s, "ok "):
		return fg(cOK).Render("ok") + fg(cText).Render(s[2:])
	}
	return fg(cText).Render(s)
}

// pane draws a box of exactly w×h cells with title set into the top border.
func (m *tui) pane(title string, lines []string, w, h int, active bool) string {
	border, titleStyle := fg(cLine), fg(cDim)
	if active {
		border, titleStyle = fg(cAmber), fg(cAmber).Bold(true)
	}
	inner := w - 4 // borders and one space of padding on each side
	title = ansi.Truncate(title, max(0, w-6), "…")
	top := border.Render("╭─ ") + titleStyle.Render(title) + " " +
		border.Render(strings.Repeat("─", max(0, w-5-lipgloss.Width(title)))+"╮")

	var b strings.Builder
	b.WriteString(top)
	for i := range max(0, h-2) {
		line := ""
		if i < len(lines) {
			line = ansi.Truncate(lines[i], inner, "…")
		}
		b.WriteString("\n" + border.Render("│") + " " + padRight(line, inner) + " " + border.Render("│"))
	}
	b.WriteString("\n" + border.Render("╰"+strings.Repeat("─", max(0, w-2))+"╯"))
	return b.String()
}

// resultLook returns the glyph, label and colour for a build.
func resultLook(b Build, running bool) (string, string, interface{ RGBA() (r, g, b, a uint32) }) {
	switch {
	case running:
		return "●", "running", cRun
	case b.Result == resultPassed:
		return "✓", "passed", cOK
	case b.Result == resultFailed:
		return "✗", "failed", cBad
	case b.Result == resultError:
		return "✗", "error", cBad
	case b.Result == resultNoTask:
		return "–", "no ci task", cDim
	case b.Result == "":
		return "○", "queued", cDim
	}
	return "·", b.Result, cDim
}

func buildTime(b Build) time.Time {
	if !b.Started.IsZero() {
		return b.Started
	}
	return b.Finished
}

// ago formats how long ago t was, in the largest whole unit: 5s, 3m, 2h, 4d, 2w.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dw", int(d.Hours()/24/7))
}

// clock formats a running duration as m:ss.
func clock(d time.Duration) string {
	d = max(0, d).Truncate(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func padRight(s string, w int) string { return s + strings.Repeat(" ", max(0, w-lipgloss.Width(s))) }
func padLeft(s string, w int) string  { return strings.Repeat(" ", max(0, w-lipgloss.Width(s))) + s }
