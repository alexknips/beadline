package cli

import (
	"bytes"
	"strings"
	"testing"
)

func run(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"no args prints usage to stderr", nil, ExitUsage, "", "Usage:"},
		{"help", []string{"help"}, ExitOK, "Commands:", ""},
		{"-h", []string{"-h"}, ExitOK, "Commands:", ""},
		{"--help", []string{"--help"}, ExitOK, "Commands:", ""},
		{"unknown command", []string{"frobnicate"}, ExitUsage, "", `unknown command "frobnicate"`},
		{"version", []string{"version"}, ExitOK, "beadline ", ""},
		{"forecast stub", []string{"forecast"}, ExitFailure, "", "beadline forecast: not implemented yet"},
		{"render stub", []string{"render"}, ExitFailure, "", "beadline render: not implemented yet"},
		{"serve stub", []string{"serve"}, ExitFailure, "", "beadline serve: not implemented yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := run(tt.args...)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", code, tt.wantCode)
			}
			if !strings.Contains(stdout, tt.wantStdout) || (tt.wantStdout == "" && stdout != "") {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tt.wantStdout)
			}
			if !strings.Contains(stderr, tt.wantStderr) || (tt.wantStderr == "" && stderr != "") {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.wantStderr)
			}
		})
	}
}

func TestUsageListsEveryCommand(t *testing.T) {
	_, stdout, _ := run("help")
	for _, c := range commands() {
		if !strings.Contains(stdout, "  "+c.name+" ") {
			t.Errorf("usage does not list %q:\n%s", c.name, stdout)
		}
	}
}

func TestVersion(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	Version = "v1.2.3"
	if _, stdout, _ := run("version"); stdout != "beadline v1.2.3\n" {
		t.Errorf("with linker version: stdout = %q", stdout)
	}

	Version = ""
	if got := version(); got == "" {
		t.Error("version() is empty without a linker version; want a module version or \"dev\"")
	}
}
