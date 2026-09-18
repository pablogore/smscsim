package main

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Simulator is the narrow seam the TUI drives the simulator through. It holds only what the
// three panes need: the session list they display and the MO send the form submits. *Smsc
// already satisfies it, and keeping the model off the concrete type is what lets every spec run
// against a double instead of a real SMPP server.
type Simulator interface {
	SessionSnapshot() []SessionInfo
	SendMoMessage(sender, recipient, message, systemId string) error
}

// *Smsc already satisfies the seam; this keeps that true at compile time.
var _ Simulator = (*Smsc)(nil)

const (
	// eventLogCapacity bounds the event log. A simulator can stay up for days, so the log is a
	// ring buffer rather than an ever-growing slice: past this many entries the oldest one is
	// overwritten.
	eventLogCapacity = 500

	// eventQueueCapacity bounds the hand-off buffer between the connection goroutines and the
	// render loop. Past this many pending events the sink drops rather than blocks.
	eventQueueCapacity = 256

	// sessionRefreshInterval is how often the sessions pane re-reads the snapshot.
	sessionRefreshInterval = time.Second

	// minRenderWidth and minRenderHeight are the sizes the view falls back to before the
	// terminal has reported its own, which is the zero-size state Bubbletea starts in.
	minRenderWidth  = 60
	minRenderHeight = 20
)

var (
	// ErrFormFieldRequired reports an MO form submitted with a blank field. The model rejects it
	// before the simulator is ever called.
	ErrFormFieldRequired = errors.New("sender, recipient and message are all required")

	// ErrNoSessionSelected reports an MO form submitted with no session to deliver to.
	ErrNoSessionSelected = errors.New("no bound session is selected")
)

// Adaptive colours so the interface stays legible on both light and dark terminals.
var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#4C3AC4", Dark: "#B4A0FF"}
	colorMuted  = lipgloss.AdaptiveColor{Light: "#5C6370", Dark: "#9AA0A6"}
	colorFailed = lipgloss.AdaptiveColor{Light: "#B3261E", Dark: "#FF8A80"}
	colorOk     = lipgloss.AdaptiveColor{Light: "#1B5E20", Dark: "#A5D6A7"}
)

var (
	paneTitleStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorMuted)
	focusedTitle     = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	fieldLabelStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	failureStyle     = lipgloss.NewStyle().Foreground(colorFailed)
	successStyle     = lipgloss.NewStyle().Foreground(colorOk)
	helpStyle        = lipgloss.NewStyle().Foreground(colorMuted)
	eventEntryStyle  = lipgloss.NewStyle().Foreground(colorMuted)
	emptyNoticeStyle = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
)

// focusPane names the pane that currently receives key presses.
type focusPane int

const (
	paneSessions focusPane = iota
	paneEvents
	paneForm
	paneCount
)

// formField names one MO form input.
type formField int

const (
	fieldSender formField = iota
	fieldRecipient
	fieldMessage
	fieldCount
)

// Messages the model reacts to.
type (
	// sessionTickMsg fires on the periodic refresh timer.
	sessionTickMsg struct{}

	// sessionsMsg carries a freshly read session snapshot.
	sessionsMsg []SessionInfo

	// eventMsg carries one simulator event drained from the queue.
	eventMsg Event

	// moResultMsg carries the outcome of one MO submission.
	moResultMsg struct {
		systemId string
		err      error
	}
)

// eventQueue is the non-blocking hand-off between the connection goroutines that emit events and
// the render loop that displays them.
//
// Sink is called synchronously on the connection goroutine, so it must never block: a full
// buffer means the renderer is behind, and dropping a log line is always preferable to stalling
// the SMPP state machine for a frame.
type eventQueue struct {
	events  chan Event
	dropped atomic.Uint64
}

func newEventQueue(capacity int) *eventQueue {
	return &eventQueue{events: make(chan Event, capacity)}
}

// Sink satisfies EventSink. It never blocks and never fails; a surplus event is counted and
// discarded.
func (q *eventQueue) Sink(event Event) {
	select {
	case q.events <- event:
	default:
		q.dropped.Add(1)
	}
}

// Dropped reports how many events were discarded because the buffer was full.
func (q *eventQueue) Dropped() uint64 {
	return q.dropped.Load()
}

// eventRing is a fixed-capacity ring buffer of events. Once full, pushing overwrites the oldest
// entry, so a long-running simulator keeps a bounded amount of log in memory.
type eventRing struct {
	entries []Event
	start   int
	count   int
}

func newEventRing(capacity int) eventRing {
	return eventRing{entries: make([]Event, capacity)}
}

func (r *eventRing) push(event Event) {
	capacity := len(r.entries)
	if capacity == 0 {
		return
	}
	r.entries[(r.start+r.count)%capacity] = event
	if r.count < capacity {
		r.count++
		return
	}
	r.start = (r.start + 1) % capacity
}

func (r eventRing) len() int { return r.count }

// all returns the buffered events from oldest to newest.
func (r eventRing) all() []Event {
	out := make([]Event, 0, r.count)
	for i := 0; i < r.count; i++ {
		out = append(out, r.entries[(r.start+i)%len(r.entries)])
	}
	return out
}

// TuiModel is the Bubbletea model behind the three simulator panes.
type TuiModel struct {
	simulator Simulator
	queue     *eventQueue

	focus  focusPane
	width  int
	height int

	sessions      []SessionInfo
	sessionsTable table.Model

	events eventRing

	inputs      []textinput.Model
	activeInput formField

	formErr    error
	formStatus string
}

// NewTuiModel builds the model together with the EventSink to register on the simulator with
// SetEventSink. The sink is returned rather than installed so the caller keeps control of the
// simulator's lifecycle.
func NewTuiModel(simulator Simulator) (TuiModel, EventSink) {
	queue := newEventQueue(eventQueueCapacity)

	sessionsTable := table.New(
		table.WithColumns(sessionColumns(minRenderWidth)),
		table.WithRows(nil),
		table.WithFocused(true),
		table.WithHeight(6),
	)

	inputs := make([]textinput.Model, fieldCount)
	for field := range inputs {
		input := textinput.New()
		input.Prompt = ""
		inputs[field] = input
	}
	inputs[fieldSender].Placeholder = "sender"
	inputs[fieldRecipient].Placeholder = "recipient"
	inputs[fieldMessage].Placeholder = "message"
	inputs[fieldMessage].CharLimit = 1024

	model := TuiModel{
		simulator:     simulator,
		queue:         queue,
		focus:         paneSessions,
		sessionsTable: sessionsTable,
		events:        newEventRing(eventLogCapacity),
		inputs:        inputs,
	}

	return model, queue.Sink
}

// Init starts the first snapshot read, the refresh timer and the event drain.
func (m TuiModel) Init() tea.Cmd {
	return tea.Batch(refreshSessions(m.simulator), scheduleSessionRefresh(), waitForEvent(m.queue))
}

// scheduleSessionRefresh re-arms the periodic snapshot read.
func scheduleSessionRefresh() tea.Cmd {
	return tea.Tick(sessionRefreshInterval, func(time.Time) tea.Msg { return sessionTickMsg{} })
}

// refreshSessions re-reads the session snapshot off the render loop.
func refreshSessions(simulator Simulator) tea.Cmd {
	return func() tea.Msg { return sessionsMsg(simulator.SessionSnapshot()) }
}

// waitForEvent drains one event from the queue. It blocks inside the command, never inside
// Update, so the render loop stays responsive.
func waitForEvent(queue *eventQueue) tea.Cmd {
	return func() tea.Msg { return eventMsg(<-queue.events) }
}

// sendMoMessage submits one MO message off the render loop.
func sendMoMessage(simulator Simulator, sender, recipient, message, systemId string) tea.Cmd {
	return func() tea.Msg {
		return moResultMsg{
			systemId: systemId,
			err:      simulator.SendMoMessage(sender, recipient, message, systemId),
		}
	}
}

// Update advances the model. Every branch returns the next model and the command it wants run.
func (m TuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		return m.updateKey(msg)

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case sessionTickMsg:
		return m, tea.Batch(refreshSessions(m.simulator), scheduleSessionRefresh())

	case sessionsMsg:
		m.sessions = []SessionInfo(msg)
		m.sessionsTable.SetRows(sessionRows(m.sessions))
		if cursor := m.sessionsTable.Cursor(); cursor >= len(m.sessions) {
			m.sessionsTable.SetCursor(0)
		}
		return m, nil

	case eventMsg:
		m.events.push(Event(msg))
		return m, waitForEvent(m.queue)

	case moResultMsg:
		if msg.err != nil {
			m.formErr = msg.err
			m.formStatus = ""
			return m, nil
		}
		m.formErr = nil
		m.formStatus = fmt.Sprintf("MO message sent to %s", msg.systemId)
		m.resetInputs()
		return m, nil
	}

	return m, nil
}

// updateKey routes one key press. Ctrl+C is handled before anything else so it quits from every
// pane, including while a text input holds the focus.
func (m TuiModel) updateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlC {
		return m, tea.Quit
	}

	switch msg.Type {
	case tea.KeyTab:
		m.setFocus((m.focus + 1) % paneCount)
		return m, nil
	case tea.KeyShiftTab:
		m.setFocus((m.focus + paneCount - 1) % paneCount)
		return m, nil
	}

	// While the MO form has focus every remaining key belongs to the text inputs. A "q" typed
	// there is text, not a quit request.
	if m.focus == paneForm {
		return m.updateForm(msg)
	}

	if msg.Type == tea.KeyRunes && string(msg.Runes) == "q" {
		return m, tea.Quit
	}

	if m.focus == paneSessions {
		var cmd tea.Cmd
		m.sessionsTable, cmd = m.sessionsTable.Update(msg)
		return m, cmd
	}

	return m, nil
}

// updateForm handles the keys of the MO form: up and down move between fields, enter submits and
// everything else is text.
func (m TuiModel) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		return m.submitForm()
	case tea.KeyUp:
		m.focusInput(m.activeInput - 1)
		return m, nil
	case tea.KeyDown:
		m.focusInput(m.activeInput + 1)
		return m, nil
	}

	var cmd tea.Cmd
	m.inputs[m.activeInput], cmd = m.inputs[m.activeInput].Update(msg)
	return m, cmd
}

// submitForm validates the form in the model and only then reaches the simulator, so a blank
// field never costs a call.
func (m TuiModel) submitForm() (tea.Model, tea.Cmd) {
	sender := strings.TrimSpace(m.inputs[fieldSender].Value())
	recipient := strings.TrimSpace(m.inputs[fieldRecipient].Value())
	message := strings.TrimSpace(m.inputs[fieldMessage].Value())

	if sender == "" || recipient == "" || message == "" {
		m.formErr = ErrFormFieldRequired
		m.formStatus = ""
		return m, nil
	}

	session, selected := m.selectedSession()
	if !selected {
		m.formErr = ErrNoSessionSelected
		m.formStatus = ""
		return m, nil
	}

	m.formErr = nil
	m.formStatus = ""
	return m, sendMoMessage(m.simulator, sender, recipient, message, session.SystemId)
}

// selectedSession returns the session the sessions table currently points at.
func (m TuiModel) selectedSession() (SessionInfo, bool) {
	cursor := m.sessionsTable.Cursor()
	if cursor < 0 || cursor >= len(m.sessions) {
		return SessionInfo{}, false
	}
	return m.sessions[cursor], true
}

// setFocus moves the focus between panes and keeps the widget focus states in step.
func (m *TuiModel) setFocus(pane focusPane) {
	m.focus = pane

	if pane == paneSessions {
		m.sessionsTable.Focus()
	} else {
		m.sessionsTable.Blur()
	}

	if pane == paneForm {
		m.focusInput(m.activeInput)
		return
	}
	for field := range m.inputs {
		m.inputs[field].Blur()
	}
}

// focusInput focuses one MO form field and blurs the rest, wrapping at both ends.
func (m *TuiModel) focusInput(field formField) {
	if field < 0 {
		field = fieldCount - 1
	}
	if field >= fieldCount {
		field = 0
	}
	m.activeInput = field

	for i := range m.inputs {
		if formField(i) == field {
			m.inputs[i].Focus()
			continue
		}
		m.inputs[i].Blur()
	}
}

// resetInputs empties the MO form after a successful send.
func (m *TuiModel) resetInputs() {
	for field := range m.inputs {
		m.inputs[field].Reset()
	}
	m.activeInput = fieldSender
	if m.focus == paneForm {
		m.focusInput(fieldSender)
	}
}

// layout resizes the widgets to the current terminal size.
func (m *TuiModel) layout() {
	width := m.renderWidth()
	m.sessionsTable.SetWidth(width)
	m.sessionsTable.SetColumns(sessionColumns(width))

	tableHeight := (m.renderHeight() - 14) / 2
	if tableHeight < 3 {
		tableHeight = 3
	}
	m.sessionsTable.SetHeight(tableHeight)

	inputWidth := width - 14
	if inputWidth < 10 {
		inputWidth = 10
	}
	for field := range m.inputs {
		m.inputs[field].Width = inputWidth
	}
}

func (m TuiModel) renderWidth() int {
	if m.width < minRenderWidth {
		return minRenderWidth
	}
	return m.width
}

func (m TuiModel) renderHeight() int {
	if m.height < minRenderHeight {
		return minRenderHeight
	}
	return m.height
}

// sessionColumns sizes the sessions table columns against the available width.
func sessionColumns(width int) []table.Column {
	// Five columns plus the table's own padding; the remainder goes to the remote address.
	remote := width - 52
	if remote < 12 {
		remote = 12
	}
	return []table.Column{
		{Title: "SYSTEM ID", Width: 16},
		{Title: "BIND", Width: 12},
		{Title: "REMOTE", Width: remote},
		{Title: "BOUND AT", Width: 10},
		{Title: "MO", Width: 4},
	}
}

// sessionRows projects the snapshot into table rows.
func sessionRows(sessions []SessionInfo) []table.Row {
	rows := make([]table.Row, 0, len(sessions))
	for _, session := range sessions {
		rows = append(rows, table.Row{
			session.SystemId,
			string(session.BindType),
			session.RemoteAddr,
			session.BoundAt.UTC().Format("15:04:05"),
			yesNo(session.ReceiveMo),
		})
	}
	return rows
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

// View renders the three panes. It is safe to call before the terminal has reported its size.
func (m TuiModel) View() string {
	sections := []string{
		m.sessionsView(),
		m.eventsView(),
		m.formView(),
		helpStyle.Render("tab/shift+tab switch pane - enter sends the MO message - q or ctrl+c quits"),
	}
	return strings.Join(sections, "\n\n")
}

func (m TuiModel) paneTitle(pane focusPane, title string) string {
	if m.focus == pane {
		return focusedTitle.Render("> " + title)
	}
	return paneTitleStyle.Render("  " + title)
}

func (m TuiModel) sessionsView() string {
	body := m.sessionsTable.View()
	if len(m.sessions) == 0 {
		body = emptyNoticeStyle.Render("no bound sessions")
	}
	return m.paneTitle(paneSessions, "SESSIONS") + "\n" + body
}

func (m TuiModel) eventsView() string {
	title := m.paneTitle(paneEvents, "EVENTS")

	entries := m.events.all()
	if len(entries) == 0 {
		return title + "\n" + emptyNoticeStyle.Render("no events yet")
	}

	// Only the newest entries that fit are rendered, so the log always shows the latest activity.
	visible := (m.renderHeight() - 14) / 2
	if visible < 3 {
		visible = 3
	}
	if len(entries) > visible {
		entries = entries[len(entries)-visible:]
	}

	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, eventEntryStyle.Render(formatEvent(entry)))
	}
	return title + "\n" + strings.Join(lines, "\n")
}

// formatEvent renders one log line. Only values carried by the event itself are used, so the
// output is a function of the event and nothing else.
func formatEvent(event Event) string {
	return fmt.Sprintf(
		"%s  %-12s %-16s cmd=0x%08x seq=%d %s",
		event.At.UTC().Format("15:04:05"),
		event.Kind,
		event.SystemId,
		event.CommandId,
		event.SequenceNum,
		event.Detail,
	)
}

func (m TuiModel) formView() string {
	target := "no session selected"
	if session, selected := m.selectedSession(); selected {
		target = session.SystemId
	}

	lines := []string{
		m.paneTitle(paneForm, "MO MESSAGE to "+target),
		fieldLabelStyle.Render("  sender    ") + m.inputs[fieldSender].View(),
		fieldLabelStyle.Render("  recipient ") + m.inputs[fieldRecipient].View(),
		fieldLabelStyle.Render("  message   ") + m.inputs[fieldMessage].View(),
	}

	switch {
	case m.formErr != nil:
		lines = append(lines, failureStyle.Render("  "+m.formErr.Error()))
	case m.formStatus != "":
		lines = append(lines, successStyle.Render("  "+m.formStatus))
	}

	return strings.Join(lines, "\n")
}
