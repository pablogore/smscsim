package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/getsyntegrity/go-specs/mock"
	"github.com/getsyntegrity/go-specs/specs"
)

// clearTuiEnv gives every spec the same empty environment baseline, so a spec that proves an
// environment variable is honoured is the only one that sets it.
func clearTuiEnv(t *testing.T) {
	t.Setenv("SMSC_PORT", "")
	t.Setenv("FAILED_SUBMITS", "")
}

// errLaunchFailed stands for any failure Bubbletea reports when it gives the terminal back.
var errLaunchFailed = errors.New("terminal handover failed")

// stubLauncher stands at the only boundary the specs must not cross: the blocking call that
// takes over the terminal. It records that it was reached instead of starting Bubbletea.
type stubLauncher struct {
	mock *mock.Mock
	err  error
}

func (l *stubLauncher) launch(TuiModel) error {
	l.mock.Spy("launch").Call()
	return l.err
}

func newStubLauncher(err error) *stubLauncher {
	return &stubLauncher{mock: mock.New(), err: err}
}

func TestTuiCommand(t *testing.T) {
	specs.Describe(t, "the tui command", func(s *specs.Spec) {
		s.BeforeEach(func(ctx *specs.Context) {
			clearTuiEnv(ctx.T)
		})

		s.Describe("its configuration", func(s *specs.Spec) {
			s.When("neither a flag nor an environment variable is given", func(s *specs.Spec) {
				s.It("listens on the default smpp port and answers submit_sm normally", func(ctx *specs.Context) {
					cfg, err := parseTuiConfig(nil, io.Discard)

					ctx.Expect(err == nil).To(specs.BeTrue())
					specs.ExpectT(ctx, cfg.smppPort).ToEqual(defaultSmscPort)
					ctx.Expect(cfg.failedSubmits).To(specs.BeFalse())
				})
			})

			s.When("only the environment is set", func(s *specs.Spec) {
				s.It("takes the smpp port and the failed-submits toggle from it", func(ctx *specs.Context) {
					ctx.T.Setenv("SMSC_PORT", "9999")
					ctx.T.Setenv("FAILED_SUBMITS", "true")

					cfg, err := parseTuiConfig(nil, io.Discard)

					ctx.Expect(err == nil).To(specs.BeTrue())
					specs.ExpectT(ctx, cfg.smppPort).ToEqual(9999)
					ctx.Expect(cfg.failedSubmits).To(specs.BeTrue())
				})
			})

			s.When("a flag contradicts the environment", func(s *specs.Spec) {
				s.It("lets the flag win", func(ctx *specs.Context) {
					ctx.T.Setenv("SMSC_PORT", "9999")
					ctx.T.Setenv("FAILED_SUBMITS", "true")

					cfg, err := parseTuiConfig([]string{"-smpp-port", "2777", "-failed-submits=false"}, io.Discard)

					ctx.Expect(err == nil).To(specs.BeTrue())
					specs.ExpectT(ctx, cfg.smppPort).ToEqual(2777)
					ctx.Expect(cfg.failedSubmits).To(specs.BeFalse())
				})
			})

			s.When("the environment holds a port that is not a number", func(s *specs.Spec) {
				s.It("refuses to start", func(ctx *specs.Context) {
					ctx.T.Setenv("SMSC_PORT", "not-a-port")

					_, err := parseTuiConfig(nil, io.Discard)

					ctx.Expect(err == nil).To(specs.BeFalse())
				})

				s.It("ignores the unusable value when a flag overrides it", func(ctx *specs.Context) {
					ctx.T.Setenv("SMSC_PORT", "not-a-port")

					cfg, err := parseTuiConfig([]string{"-smpp-port", "2777"}, io.Discard)

					ctx.Expect(err == nil).To(specs.BeTrue())
					specs.ExpectT(ctx, cfg.smppPort).ToEqual(2777)
				})
			})

			s.When("the port flag is out of range", func(s *specs.Spec) {
				s.It("refuses to start", func(ctx *specs.Context) {
					_, err := parseTuiConfig([]string{"-smpp-port", "0"}, io.Discard)

					ctx.Expect(err == nil).To(specs.BeFalse())
				})
			})

			s.When("the port flag is not a number at all", func(s *specs.Spec) {
				s.It("refuses to start", func(ctx *specs.Context) {
					_, err := parseTuiConfig([]string{"-smpp-port", "nope"}, io.Discard)

					ctx.Expect(err == nil).To(specs.BeFalse())
				})
			})

			s.When("help is requested", func(s *specs.Spec) {
				s.It("reports it as a help request rather than as a failure", func(ctx *specs.Context) {
					_, err := parseTuiConfig([]string{"-h"}, io.Discard)

					ctx.Expect(errors.Is(err, flag.ErrHelp)).To(specs.BeTrue())
				})
			})

			s.When("the web port flag of the serve command is passed", func(s *specs.Spec) {
				s.It("rejects it, because the tui does not start the web server", func(ctx *specs.Context) {
					_, err := parseTuiConfig([]string{"-web-port", "12776"}, io.Discard)

					ctx.Expect(err == nil).To(specs.BeFalse())
				})
			})
		})

		s.Describe("its entry point", func(s *specs.Spec) {
			s.When("the requested smpp port is already taken", func(s *specs.Spec) {
				s.It("reports the bind failure on stderr, exits non-zero and never reaches the terminal", func(ctx *specs.Context) {
					occupied, err := net.Listen("tcp", "127.0.0.1:0")
					ctx.Expect(err == nil).To(specs.BeTrue())
					defer occupied.Close()

					_, portStr, err := net.SplitHostPort(occupied.Addr().String())
					ctx.Expect(err == nil).To(specs.BeTrue())
					port, err := strconv.Atoi(portStr)
					ctx.Expect(err == nil).To(specs.BeTrue())

					launcher := newStubLauncher(nil)
					var stderr bytes.Buffer

					code := func() int {
						defer func() {
							ctx.Expect(recover()).To(specs.BeNil())
						}()
						return runTui(tuiConfig{smppPort: port}, &stderr, launcher.launch)
					}()

					ctx.Expect(code != 0).To(specs.BeTrue())
					ctx.Expect(strings.Contains(stderr.String(), "cannot listen on smpp port")).To(specs.BeTrue())
					ctx.Expect(launcher.mock.Spy("launch").WasCalled()).To(specs.BeFalse())
				})
			})

			s.When("the smpp port is free", func(s *specs.Spec) {
				s.It("hands the model to the terminal and exits successfully", func(ctx *specs.Context) {
					launcher := newStubLauncher(nil)
					var stderr bytes.Buffer

					code := runTui(tuiConfig{smppPort: 0}, &stderr, launcher.launch)

					specs.ExpectT(ctx, code).ToEqual(0)
					specs.ExpectT(ctx, stderr.String()).ToEqual("")
					specs.ExpectT(ctx, launcher.mock.Spy("launch").CallCount()).ToEqual(1)
				})

				s.It("restores the standard logger it silenced for the lifetime of the tui", func(ctx *specs.Context) {
					before := log.Writer()

					runTui(tuiConfig{smppPort: 0}, io.Discard, newStubLauncher(nil).launch)

					ctx.Expect(log.Writer() == before).To(specs.BeTrue())
				})
			})

			s.When("the terminal handover fails", func(s *specs.Spec) {
				s.It("reports the failure on stderr and exits non-zero", func(ctx *specs.Context) {
					var stderr bytes.Buffer

					code := runTui(tuiConfig{smppPort: 0}, &stderr, newStubLauncher(errLaunchFailed).launch)

					ctx.Expect(code != 0).To(specs.BeTrue())
					ctx.Expect(stderr.Len() > 0).To(specs.BeTrue())
				})
			})
		})

		s.Describe("its place in the command line", func(s *specs.Spec) {
			s.When("tui is asked for help", func(s *specs.Spec) {
				s.It("prints its own flags and the general usage, then exits successfully", func(ctx *specs.Context) {
					var stdout, stderr bytes.Buffer

					code := run([]string{"tui", "-h"}, &stdout, &stderr)

					specs.ExpectT(ctx, code).ToEqual(0)
					ctx.Expect(strings.Contains(stdout.String(), "tui")).To(specs.BeTrue())
					ctx.Expect(strings.Contains(stderr.String(), "-smpp-port")).To(specs.BeTrue())
				})
			})

			s.When("tui is given an unusable port", func(s *specs.Spec) {
				s.It("reports it on stderr and exits non-zero without touching the terminal", func(ctx *specs.Context) {
					var stdout, stderr bytes.Buffer

					code := run([]string{"tui", "-smpp-port", "0"}, &stdout, &stderr)

					specs.ExpectT(ctx, code).ToEqual(exitUsage)
					ctx.Expect(stderr.Len() > 0).To(specs.BeTrue())
				})
			})

			s.When("the general usage is printed", func(s *specs.Spec) {
				s.It("lists the tui command among the others", func(ctx *specs.Context) {
					var usage bytes.Buffer

					printUsage(&usage)

					ctx.Expect(strings.Contains(usage.String(), "tui")).To(specs.BeTrue())
				})
			})

			s.When("no arguments are given at all", func(s *specs.Spec) {
				s.It("still resolves to serve, as the docker entrypoint relies on", func(ctx *specs.Context) {
					command, commandArgs := resolveCommand(nil)

					specs.ExpectT(ctx, command).ToEqual("serve")
					ctx.Expect(commandArgs == nil).To(specs.BeTrue())
				})
			})
		})
	})
}
