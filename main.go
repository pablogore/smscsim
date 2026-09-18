package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

const (
	defaultSmscPort = 2775
	defaultWebPort  = 12775
)

// exitUsage is the exit code used for unusable input: an unknown command or an
// invalid configuration value. It matches the exit code of the log.Fatalf call
// this entry point used before it grew subcommands.
const exitUsage = 1

// serveConfig holds everything the simulator needs to start. It is built
// separately from starting the servers so command resolution and configuration
// parsing can be exercised in tests without blocking on a listener.
type serveConfig struct {
	smppPort      int
	webPort       int
	failedSubmits bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run resolves the requested command, builds its configuration and executes it.
// It returns the process exit code instead of calling os.Exit so it stays
// testable.
func run(args []string, stdout, stderr io.Writer) int {
	command, commandArgs := resolveCommand(args)

	switch command {
	case "help":
		printUsage(stdout)
		return 0
	case "serve":
		cfg, err := parseServeConfig(commandArgs, stderr)
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		serve(cfg)
		return 0
	case "tui":
		cfg, err := parseTuiConfig(commandArgs, stderr)
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return 0
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return exitUsage
		}
		return runTui(cfg, stderr, launchTuiProgram)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", command)
		printUsage(stderr)
		return exitUsage
	}
}

// resolveCommand maps the raw arguments to a command name and the arguments
// that belong to it. No arguments means "serve", so running the binary with no
// arguments keeps behaving exactly as it did before subcommands existed (the
// docker image entrypoint relies on this). Arguments that start with a dash are
// treated as serve flags rather than as a command name.
func resolveCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "serve", nil
	}

	switch args[0] {
	case "-h", "-help", "--help":
		return "help", nil
	}

	if args[0][0] == '-' {
		return "serve", args
	}

	return args[0], args[1:]
}

// parseServeConfig builds the serve configuration. Precedence is
// flag > environment variable > built-in default.
func parseServeConfig(args []string, stderr io.Writer) (serveConfig, error) {
	// The environment values are resolved first so they can act as the flag
	// defaults. An invalid value is remembered rather than reported right away:
	// it only matters when no flag overrides it.
	smscPort, smscPortErr := portFromEnv("SMSC_PORT", defaultSmscPort)
	webPort, webPortErr := portFromEnv("WEB_PORT", defaultWebPort)

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	smppPortFlag := fs.Int("smpp-port", smscPort, "port the SMPP server listens on (env SMSC_PORT)")
	webPortFlag := fs.Int("web-port", webPort, "port the web server listens on (env WEB_PORT)")
	failedSubmitsFlag := fs.Bool("failed-submits", os.Getenv("FAILED_SUBMITS") == "true", "make submit_sm requests fail (env FAILED_SUBMITS)")

	if err := fs.Parse(args); err != nil {
		return serveConfig{}, err
	}

	explicit := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })

	if !explicit["smpp-port"] && smscPortErr != nil {
		return serveConfig{}, smscPortErr
	}
	if !explicit["web-port"] && webPortErr != nil {
		return serveConfig{}, webPortErr
	}
	if *smppPortFlag < 1 {
		return serveConfig{}, fmt.Errorf("invalid port -smpp-port [%d]", *smppPortFlag)
	}
	if *webPortFlag < 1 {
		return serveConfig{}, fmt.Errorf("invalid port -web-port [%d]", *webPortFlag)
	}

	return serveConfig{
		smppPort:      *smppPortFlag,
		webPort:       *webPortFlag,
		failedSubmits: *failedSubmitsFlag,
	}, nil
}

// serve starts the SMPP and web servers and blocks until both stop.
func serve(cfg serveConfig) {
	var wg sync.WaitGroup
	wg.Add(2)

	// start smpp server
	smsc := NewSmsc(cfg.failedSubmits)
	go smsc.Start(cfg.smppPort, &wg)

	// start web server
	webServer := NewWebServer(smsc)
	go webServer.Start(cfg.webPort, &wg)

	wg.Wait()
}

// portFromEnv reads a port from an environment variable, falling back to defVal
// when the variable is unset. An unusable value is returned as an error so the
// caller decides how to surface it.
func portFromEnv(envVar string, defVal int) (int, error) {
	portStr := os.Getenv(envVar)
	if portStr == "" {
		return defVal, nil
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 {
		return defVal, fmt.Errorf("invalid port %s [%s]", envVar, portStr)
	}

	return port, nil
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `smscsim - lightweight SMSc simulator

Usage:
  smscsim [command] [flags]

Commands:
  serve   start the simulator (SMPP and web servers). This is the default
          command when none is given.
  tui     start the simulator with a terminal interface (SMPP server only, no
          web server).
  help    show this help

Flags for serve:
  --smpp-port int        port the SMPP server listens on (env SMSC_PORT, default 2775)
  --web-port int         port the web server listens on (env WEB_PORT, default 12775)
  --failed-submits       make submit_sm requests fail (env FAILED_SUBMITS, default false)

Flags for tui:
  --smpp-port int        port the SMPP server listens on (env SMSC_PORT, default 2775)
  --failed-submits       make submit_sm requests fail (env FAILED_SUBMITS, default false)

A flag always takes precedence over its environment variable, which takes
precedence over the built-in default.
`)
}
