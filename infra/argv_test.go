package infra

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestShellArgvActualShellPreservesEmptyAndMetacharacters(t *testing.T) {
	// Local shell only: no SSH, privileged commands, files or fault injection.
	cmd, err := ShellArgv("printf", "[%s]", "", "30", "a'b", "$(false)", "line\nline")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "wsl", "-e", "bash", "-c", cmd)
	} else {
		c = exec.CommandContext(ctx, "bash", "-c", cmd)
	}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("local fake transport: %s %v", out, err)
	}
	if string(out) != "[][30][a'b][$(false)][line\nline]" {
		t.Fatalf("argv shifted or interpreted: %q", out)
	}
}

func TestShellArgvRejectsInvalidExecutableOrNUL(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"printf", "\x00"}} {
		if _, err := ShellArgv(args...); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	cmd, _ := ShellArgv("printf", "", "30")
	if !strings.Contains(cmd, "'' '30'") {
		t.Fatal("empty positional omitted")
	}
}
