package main

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/getsyntegrity/go-specs/mock"
	"github.com/getsyntegrity/go-specs/specs"
)

// fixedBoundAt and the fixture addresses below keep every rendered value deterministic. No real
// clock and no real remote address ever reach the view under test.
var fixedBoundAt = time.Date(2024, time.March, 1, 9, 30, 0, 0, time.UTC)

// ansiPattern strips the escape sequences lipgloss emits so a rendered assertion does not depend
// on the colour profile of the terminal the suite happens to run in.
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func plain(view string) string {
	return ansiPattern.ReplaceAllString(view, "")
}

// fakeSimulator is the hand-written double standing at the simulator boundary. Every spec drives
// the model through this seam, so no spec starts a real SMPP server.
type fakeSimulator struct {
	mock     *mock.Mock
	sessions []SessionInfo
	sendErr  error
}

func (f *fakeSimulator) SessionSnapshot() []SessionInfo {
	f.mock.Spy("SessionSnapshot").Call()
	return f.sessions
}

func (f *fakeSimulator) SendMoMessage(sender, recipient, message, systemId string) error {
	f.mock.Spy("SendMoMessage").Call(sender, recipient, message, systemId)
	return f.sendErr
}

func baselineSessions() []SessionInfo {
	return []SessionInfo{
		{SessionId: 1, SystemId: "alpha", BindType: BindTransceiver, BoundAt: fixedBoundAt, RemoteAddr: "198.51.100.10:41001", ReceiveMo: true},
		{SessionId: 2, SystemId: "bravo", BindType: BindTransmitter, BoundAt: fixedBoundAt, RemoteAddr: "198.51.100.11:41002", ReceiveMo: false},
	}
}

func baselineSimulator() *fakeSimulator {
	return &fakeSimulator{mock: mock.New(), sessions: baselineSessions()}
}

// newFixture builds a model that already knows its session list and its terminal size, so a spec
// only has to express the one transition it is about.
func newFixture(sim Simulator) TuiModel {
	model, _ := NewTuiModel(sim)
	next, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	next, _ = next.(TuiModel).Update(sessionsMsg(sim.SessionSnapshot()))
	return next.(TuiModel)
}

func advance(model TuiModel, msg tea.Msg) (TuiModel, tea.Cmd) {
	next, cmd := model.Update(msg)
	return next.(TuiModel), cmd
}

func key(t tea.KeyType) tea.KeyMsg { return tea.KeyMsg{Type: t} }

func typed(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}

func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, isQuit := cmd().(tea.QuitMsg)
	return isQuit
}

func fillForm(model TuiModel, sender, recipient, message string) TuiModel {
	model, _ = advance(model, key(tea.KeyTab))
	model, _ = advance(model, key(tea.KeyTab))
	model, _ = advance(model, typed(sender))
	model, _ = advance(model, key(tea.KeyDown))
	model, _ = advance(model, typed(recipient))
	model, _ = advance(model, key(tea.KeyDown))
	model, _ = advance(model, typed(message))
	return model
}

func TestSimulatorTui(t *testing.T) {
	specs.Describe(t, "the simulator TUI", func(s *specs.Spec) {
		s.When("the operator presses tab", func(s *specs.Spec) {
			s.It("cycles the focus forward through the sessions, events and MO form panes", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())
				specs.EqualTo(ctx, model.focus, paneSessions)

				model, _ = advance(model, key(tea.KeyTab))
				specs.EqualTo(ctx, model.focus, paneEvents)

				model, _ = advance(model, key(tea.KeyTab))
				specs.EqualTo(ctx, model.focus, paneForm)

				model, _ = advance(model, key(tea.KeyTab))
				specs.EqualTo(ctx, model.focus, paneSessions)
			})
		})

		s.When("the operator presses shift+tab", func(s *specs.Spec) {
			s.It("cycles the focus backwards through the panes", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				model, _ = advance(model, key(tea.KeyShiftTab))
				specs.EqualTo(ctx, model.focus, paneForm)

				model, _ = advance(model, key(tea.KeyShiftTab))
				specs.EqualTo(ctx, model.focus, paneEvents)
			})
		})

		s.When("an event reaches the model", func(s *specs.Spec) {
			s.It("appends it to the event log and waits for the next one", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				model, cmd := advance(model, eventMsg(Event{At: fixedBoundAt, Kind: EventBind, SystemId: "alpha", Detail: "bound"}))

				specs.EqualTo(ctx, model.events.len(), 1)
				specs.EqualTo(ctx, model.events.all()[0].Detail, "bound")
				ctx.Expect(cmd == nil).To(specs.BeFalse())
			})
		})

		s.When("more events arrive than the log can hold", func(s *specs.Spec) {
			s.It("keeps the newest entries and evicts the oldest", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				for i := 0; i < eventLogCapacity+5; i++ {
					model, _ = advance(model, eventMsg(Event{At: fixedBoundAt, Kind: EventInboundPdu, SequenceNum: uint32(i)}))
				}

				entries := model.events.all()
				specs.EqualTo(ctx, model.events.len(), eventLogCapacity)
				specs.EqualTo(ctx, entries[0].SequenceNum, uint32(5))
				specs.EqualTo(ctx, entries[len(entries)-1].SequenceNum, uint32(eventLogCapacity+4))
			})
		})

		s.When("the terminal reports a new size", func(s *specs.Spec) {
			s.It("adopts the size and keeps rendering", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				model, _ = advance(model, tea.WindowSizeMsg{Width: 100, Height: 30})

				specs.EqualTo(ctx, model.width, 100)
				specs.EqualTo(ctx, model.height, 30)
				ctx.Expect(len(model.View()) > 0).To(specs.BeTrue())
			})
		})

		s.When("the terminal has not reported its size yet", func(s *specs.Spec) {
			s.It("renders a view instead of panicking on the zero size", func(ctx *specs.Context) {
				model, _ := NewTuiModel(baselineSimulator())

				specs.EqualTo(ctx, model.width, 0)
				ctx.Expect(len(model.View()) > 0).To(specs.BeTrue())
			})
		})

		s.When("the MO form is submitted with every field filled", func(s *specs.Spec) {
			s.It("sends the message once against the selected session's system id", func(ctx *specs.Context) {
				sim := baselineSimulator()
				model := fillForm(newFixture(sim), "1111", "2222", "hello")

				_, cmd := advance(model, key(tea.KeyEnter))
				ctx.Expect(cmd == nil).To(specs.BeFalse())
				cmd()

				spy := sim.mock.Spy("SendMoMessage")
				ctx.Expect(spy.CalledWith(mock.Equal("1111"), mock.Equal("2222"), mock.Equal("hello"), mock.Equal("alpha"))).To(specs.BeTrue())
				specs.EqualTo(ctx, spy.CallCount(), 1)
			})
		})

		s.When("a required field of the MO form is empty", func(s *specs.Spec) {
			s.It("rejects the submission without reaching the simulator", func(ctx *specs.Context) {
				sim := baselineSimulator()
				model := fillForm(newFixture(sim), "1111", "", "hello")

				model, cmd := advance(model, key(tea.KeyEnter))

				ctx.Expect(cmd == nil).To(specs.BeTrue())
				ctx.Expect(errors.Is(model.formErr, ErrFormFieldRequired)).To(specs.BeTrue())
				ctx.Expect(sim.mock.Spy("SendMoMessage").WasCalled()).To(specs.BeFalse())
			})
		})

		s.When("no session is bound", func(s *specs.Spec) {
			s.It("rejects the submission because there is no recipient session", func(ctx *specs.Context) {
				sim := baselineSimulator()
				sim.sessions = nil
				model := fillForm(newFixture(sim), "1111", "2222", "hello")

				model, cmd := advance(model, key(tea.KeyEnter))

				ctx.Expect(cmd == nil).To(specs.BeTrue())
				ctx.Expect(errors.Is(model.formErr, ErrNoSessionSelected)).To(specs.BeTrue())
				ctx.Expect(sim.mock.Spy("SendMoMessage").WasCalled()).To(specs.BeFalse())
			})
		})

		s.When("the simulator rejects the MO message", func(s *specs.Spec) {
			s.It("surfaces the failure in the view", func(ctx *specs.Context) {
				sim := baselineSimulator()
				sim.sendErr = errors.New("no receiver session")
				model := fillForm(newFixture(sim), "1111", "2222", "hello")

				_, cmd := advance(model, key(tea.KeyEnter))
				model, _ = advance(model, cmd())

				ctx.Expect(strings.Contains(plain(model.View()), "no receiver session")).To(specs.BeTrue())
			})
		})

		s.When("the simulator accepts the MO message", func(s *specs.Spec) {
			s.It("reports the success in the view and clears the form", func(ctx *specs.Context) {
				sim := baselineSimulator()
				model := fillForm(newFixture(sim), "1111", "2222", "hello")

				_, cmd := advance(model, key(tea.KeyEnter))
				model, _ = advance(model, cmd())

				ctx.Expect(strings.Contains(plain(model.View()), "MO message sent to alpha")).To(specs.BeTrue())
				specs.EqualTo(ctx, model.inputs[fieldMessage].Value(), "")
			})
		})

		s.When("ctrl+c is pressed", func(s *specs.Spec) {
			s.It("quits from every pane, including while a text input has focus", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				for pane := paneSessions; pane < paneCount; pane++ {
					specs.EqualTo(ctx, model.focus, pane)
					_, cmd := advance(model, key(tea.KeyCtrlC))
					ctx.Expect(quits(cmd)).To(specs.BeTrue())
					model, _ = advance(model, key(tea.KeyTab))
				}
			})
		})

		s.When("q is pressed outside the MO form", func(s *specs.Spec) {
			s.It("quits the simulator TUI", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())

				_, cmd := advance(model, typed("q"))

				ctx.Expect(quits(cmd)).To(specs.BeTrue())
			})
		})

		s.When("q is typed into a focused text input", func(s *specs.Spec) {
			s.It("is treated as text rather than as a quit key", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())
				model, _ = advance(model, key(tea.KeyTab))
				model, _ = advance(model, key(tea.KeyTab))

				model, cmd := advance(model, typed("q"))

				ctx.Expect(quits(cmd)).To(specs.BeFalse())
				specs.EqualTo(ctx, model.inputs[fieldSender].Value(), "q")
			})
		})

		s.When("the session refresh tick fires", func(s *specs.Spec) {
			s.It("re-reads the session snapshot through the simulator and re-arms itself", func(ctx *specs.Context) {
				sim := baselineSimulator()
				model, _ := NewTuiModel(sim)

				_, cmd := advance(model, sessionTickMsg{})

				ctx.Expect(cmd == nil).To(specs.BeFalse())
				batch, batched := cmd().(tea.BatchMsg)
				ctx.Expect(batched).To(specs.BeTrue())
				for _, batchedCmd := range batch {
					batchedCmd()
				}

				ctx.Expect(sim.mock.Spy("SessionSnapshot").WasCalled()).To(specs.BeTrue())
				specs.EqualTo(ctx, len(batch), 2)
			})
		})

		s.When("the event sink is offered more events than its buffer holds", func(s *specs.Spec) {
			s.It("drops the surplus instead of blocking the connection goroutine", func(ctx *specs.Context) {
				queue := newEventQueue(2)

				for i := 0; i < 5; i++ {
					queue.Sink(Event{At: fixedBoundAt, Kind: EventInboundPdu, SequenceNum: uint32(i)})
				}

				specs.EqualTo(ctx, len(queue.events), 2)
				specs.EqualTo(ctx, queue.Dropped(), uint64(3))
			})
		})

		s.When("the sink handed to the simulator is saturated", func(s *specs.Spec) {
			s.It("returns to the caller without blocking and records the drop", func(ctx *specs.Context) {
				model, sink := NewTuiModel(baselineSimulator())

				for i := 0; i < eventQueueCapacity+4; i++ {
					sink(Event{At: fixedBoundAt, Kind: EventOutboundPdu, SequenceNum: uint32(i)})
				}

				specs.EqualTo(ctx, model.queue.Dropped(), uint64(4))
			})
		})

		s.When("the panes are rendered with bound sessions and a logged event", func(s *specs.Spec) {
			s.It("renders the sessions table, the event log and the MO form", func(ctx *specs.Context) {
				model := newFixture(baselineSimulator())
				model, _ = advance(model, eventMsg(Event{
					At:          fixedBoundAt,
					Kind:        EventBind,
					SystemId:    "alpha",
					CommandId:   BIND_TRANSCEIVER,
					SequenceNum: 1,
					Detail:      "bind_transceiver accepted",
				}))

				ctx.Snapshot("three panes with one bound session and one event", plain(model.View()))
			})
		})

		s.When("an operator types a full MO message and submits it", func(s *specs.Spec) {
			s.It("renders the outcome of the send", func(ctx *specs.Context) {
				t := ctx.T
				sim := baselineSimulator()
				model, _ := NewTuiModel(sim)

				tm := teatest.NewTestModel(t, model, teatest.WithInitialTermSize(120, 40))
				tm.Send(sessionsMsg(sim.sessions))
				tm.Send(key(tea.KeyTab))
				tm.Send(key(tea.KeyTab))
				tm.Send(typed("1111"))
				tm.Send(key(tea.KeyDown))
				tm.Send(typed("2222"))
				tm.Send(key(tea.KeyDown))
				tm.Send(typed("hello"))
				tm.Send(key(tea.KeyEnter))

				teatest.WaitFor(t, tm.Output(), func(out []byte) bool {
					return strings.Contains(plain(string(out)), "MO message sent to alpha")
				}, teatest.WithCheckInterval(10*time.Millisecond), teatest.WithDuration(5*time.Second))

				tm.Send(key(tea.KeyCtrlC))
				tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))

				ctx.Expect(sim.mock.Spy("SendMoMessage").CalledWith(mock.Equal("1111"), mock.Equal("2222"), mock.Equal("hello"), mock.Equal("alpha"))).To(specs.BeTrue())
			})
		})
	})
}
