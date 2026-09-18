package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"

	tea "github.com/charmbracelet/bubbletea"
)

// exitRuntime is the exit code for a failure that happens while the command is starting up
// rather than while its arguments are being read, such as an smpp port that is already taken.
// It is named apart from exitUsage so the two paths stay distinguishable in the code even
// though a shell sees the same value for both.
const exitRuntime = 1

// tuiConfig holds everything the terminal interface needs. It is built separately from starting
// the interface so parsing and validation can be exercised without a terminal: the blocking
// call that takes the terminal over is the only part no spec drives.
type tuiConfig struct {
	smppPort      int
	failedSubmits bool
}

// tuiLauncher is the boundary between the parts of the tui command that can run under go test
// and the part that cannot. The real implementation is launchTuiProgram; a spec passes a stub.
type tuiLauncher func(model TuiModel) error

// parseTuiConfig builds the terminal interface configuration. Precedence is the same as the
// serve command's: flag > environment variable > built-in default. The tui does not start the
// web server, so it has no web port flag.
func parseTuiConfig(args []string, stderr io.Writer) (tuiConfig, error) {
	// As in parseServeConfig, the environment is resolved first so it can act as the flag
	// default, and an unusable value is only reported when no flag overrides it.
	smscPort, smscPortErr := portFromEnv("SMSC_PORT", defaultSmscPort)

	fs := flag.NewFlagSet("tui", flag.ContinueOnError)
	fs.SetOutput(stderr)
	smppPortFlag := fs.Int("smpp-port", smscPort, "port the SMPP server listens on (env SMSC_PORT)")
	failedSubmitsFlag := fs.Bool("failed-submits", os.Getenv("FAILED_SUBMITS") == "true", "make submit_sm requests fail (env FAILED_SUBMITS)")

	if err := fs.Parse(args); err != nil {
		return tuiConfig{}, err
	}

	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if !explicit["smpp-port"] && smscPortErr != nil {
		return tuiConfig{}, smscPortErr
	}
	if *smppPortFlag < 1 {
		return tuiConfig{}, fmt.Errorf("invalid port -smpp-port [%d]", *smppPortFlag)
	}

	return tuiConfig{
		smppPort:      *smppPortFlag,
		failedSubmits: *failedSubmitsFlag,
	}, nil
}

// runTui starts the SMPP server and hands the model to launch, which owns the terminal until
// the user quits.
//
// It deliberately does not call Smsc.Start: that helper reports a failed listen with log.Panic,
// which would tear the process down without giving Bubbletea the chance to restore the
// terminal, leaving the user's shell in raw mode. The listener is opened here instead, so a
// port that is already taken is an ordinary error on stderr with a non-zero exit code, reported
// before anything has been drawn.
func runTui(cfg tuiConfig, stderr io.Writer, launch tuiLauncher) int {
	ln, err := net.Listen("tcp", fmt.Sprint(":", cfg.smppPort))
	if err != nil {
		fmt.Fprintf(stderr, "cannot listen on smpp port %d: %v\n", cfg.smppPort, err)
		return exitRuntime
	}
	defer ln.Close()

	smsc := NewSmsc(cfg.failedSubmits)
	model, sink := NewTuiModel(smsc)
	smsc.SetEventSink(sink)
	go smsc.serve(ln)

	// The standard logger writes to stderr, which is the very terminal the interface draws on,
	// so every log.Printf in the SMPP state machine would scribble over the rendered frame.
	// Those messages are not lost to the user: the event sink above feeds the same protocol
	// activity into the events pane. The previous output is restored on the way out, so a
	// failure reported after this point still reaches stderr.
	previousLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(previousLogOutput)

	if err := launch(model); err != nil {
		log.SetOutput(previousLogOutput)
		fmt.Fprintln(stderr, err)
		return exitRuntime
	}

	return 0
}

// launchTuiProgram is the real launcher: it blocks until the user quits the interface. It needs
// a real terminal, which is why it is kept behind the tuiLauncher seam.
func launchTuiProgram(model TuiModel) error {
	_, err := tea.NewProgram(model, tea.WithAltScreen()).Run()
	return err
}
