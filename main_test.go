package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
)

func TestResolveCommandWithoutArgumentsIsServe(t *testing.T) {
	name, rest := resolveCommand(nil)
	if name != "serve" {
		t.Errorf("expected command [serve], got [%s]", name)
	}
	if len(rest) != 0 {
		t.Errorf("expected no remaining arguments, got %v", rest)
	}
}

func TestResolveCommandWithExplicitServeIsServe(t *testing.T) {
	name, rest := resolveCommand([]string{"serve", "--smpp-port", "1234"})
	if name != "serve" {
		t.Errorf("expected command [serve], got [%s]", name)
	}
	if len(rest) != 2 || rest[0] != "--smpp-port" || rest[1] != "1234" {
		t.Errorf("expected serve flags to be forwarded, got %v", rest)
	}
}

func TestResolveCommandWithLeadingFlagsIsServe(t *testing.T) {
	// keeps `./smscsim --smpp-port 1234` working without naming the subcommand
	name, rest := resolveCommand([]string{"--smpp-port", "1234"})
	if name != "serve" {
		t.Errorf("expected command [serve], got [%s]", name)
	}
	if len(rest) != 2 || rest[0] != "--smpp-port" {
		t.Errorf("expected leading flags to be forwarded to serve, got %v", rest)
	}
}

func TestResolveCommandWithHelpFlagsIsHelp(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		name, _ := resolveCommand([]string{arg})
		if name != "help" {
			t.Errorf("expected [%s] to resolve to command [help], got [%s]", arg, name)
		}
	}
}

func TestResolveCommandWithUnknownCommandKeepsItsName(t *testing.T) {
	name, _ := resolveCommand([]string{"bogus"})
	if name != "bogus" {
		t.Errorf("expected unknown command name to be preserved, got [%s]", name)
	}
}

func TestRunHelpPrintsUsageAndExitsZero(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"help"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}
	out := stdout.String()
	for _, want := range []string{"Usage", "serve", "help"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected usage output to mention [%s], got:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("expected empty stderr, got:\n%s", stderr.String())
	}
}

func TestRunUnknownCommandNamesItAndExitsNonZero(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := run([]string{"bogus"}, &stdout, &stderr)

	if code == 0 {
		t.Error("expected a non-zero exit code for an unknown command")
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, "bogus") {
		t.Errorf("expected error output to name the offending command, got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "Usage") {
		t.Errorf("expected error output to include the usage, got:\n%s", errOut)
	}
}

func TestParseServeConfigUsesDefaultsWhenNothingIsSet(t *testing.T) {
	t.Setenv("SMSC_PORT", "")
	t.Setenv("WEB_PORT", "")
	t.Setenv("FAILED_SUBMITS", "")

	cfg, err := parseServeConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.smppPort != 2775 {
		t.Errorf("expected default smpp port 2775, got %d", cfg.smppPort)
	}
	if cfg.webPort != 12775 {
		t.Errorf("expected default web port 12775, got %d", cfg.webPort)
	}
	if cfg.failedSubmits {
		t.Error("expected failed submits to be disabled by default")
	}
}

func TestParseServeConfigHonoursEnvWhenNoFlagIsGiven(t *testing.T) {
	t.Setenv("SMSC_PORT", "9999")
	t.Setenv("WEB_PORT", "8888")
	t.Setenv("FAILED_SUBMITS", "true")

	cfg, err := parseServeConfig(nil, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.smppPort != 9999 {
		t.Errorf("expected smpp port 9999 from env, got %d", cfg.smppPort)
	}
	if cfg.webPort != 8888 {
		t.Errorf("expected web port 8888 from env, got %d", cfg.webPort)
	}
	if !cfg.failedSubmits {
		t.Error("expected failed submits to be enabled from env")
	}
}

func TestParseServeConfigFlagOverridesEnv(t *testing.T) {
	t.Setenv("SMSC_PORT", "9999")
	t.Setenv("WEB_PORT", "8888")
	t.Setenv("FAILED_SUBMITS", "true")

	cfg, err := parseServeConfig([]string{
		"--smpp-port", "1234",
		"--web-port", "5678",
		"--failed-submits=false",
	}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.smppPort != 1234 {
		t.Errorf("expected flag to override env smpp port, got %d", cfg.smppPort)
	}
	if cfg.webPort != 5678 {
		t.Errorf("expected flag to override env web port, got %d", cfg.webPort)
	}
	if cfg.failedSubmits {
		t.Error("expected flag to override env failed submits")
	}
}

func TestParseServeConfigRejectsInvalidEnvPort(t *testing.T) {
	t.Setenv("SMSC_PORT", "not-a-port")

	_, err := parseServeConfig(nil, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an invalid env port")
	}
	if !strings.Contains(err.Error(), "SMSC_PORT") || !strings.Contains(err.Error(), "not-a-port") {
		t.Errorf("expected the error to name the variable and the value, got: %v", err)
	}
}

func TestParseServeConfigRejectsOutOfRangeEnvPort(t *testing.T) {
	t.Setenv("WEB_PORT", "0")

	_, err := parseServeConfig(nil, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an out-of-range env port")
	}
	if !strings.Contains(err.Error(), "WEB_PORT") {
		t.Errorf("expected the error to name the variable, got: %v", err)
	}
}

func TestParseServeConfigIgnoresInvalidEnvPortWhenFlagOverridesIt(t *testing.T) {
	t.Setenv("SMSC_PORT", "not-a-port")

	cfg, err := parseServeConfig([]string{"--smpp-port", "1234"}, io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.smppPort != 1234 {
		t.Errorf("expected the flag to take precedence over the invalid env value, got %d", cfg.smppPort)
	}
}

func TestParseServeConfigRejectsInvalidFlagPort(t *testing.T) {
	_, err := parseServeConfig([]string{"--smpp-port", "0"}, io.Discard)
	if err == nil {
		t.Fatal("expected an error for an out-of-range flag port")
	}
	if !strings.Contains(err.Error(), "smpp-port") {
		t.Errorf("expected the error to name the flag, got: %v", err)
	}
}

func TestParseServeConfigReportsHelpRequest(t *testing.T) {
	_, err := parseServeConfig([]string{"-h"}, io.Discard)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got: %v", err)
	}
}

func TestRunServeWithInvalidConfigExitsNonZeroWithoutStartingServers(t *testing.T) {
	t.Setenv("SMSC_PORT", "not-a-port")
	var stdout, stderr bytes.Buffer

	code := run([]string{"serve"}, &stdout, &stderr)

	if code == 0 {
		t.Error("expected a non-zero exit code for an invalid configuration")
	}
	if !strings.Contains(stderr.String(), "SMSC_PORT") {
		t.Errorf("expected the error on stderr, got:\n%s", stderr.String())
	}
}
