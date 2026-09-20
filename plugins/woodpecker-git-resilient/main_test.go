package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRetryEventuallySucceeds(t *testing.T) {
	var attempts int
	err := retry(3, 0, func(n int) error {
		attempts = n
		if n < 3 {
			return errors.New("temporary fetch failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry returned error: %v", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestRetryReturnsLastError(t *testing.T) {
	want := errors.New("permanent fetch failure")
	var attempts int
	err := retry(2, 0, func(n int) error {
		attempts = n
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestRunCommandIdleTimeoutKillsProcess(t *testing.T) {
	var output bytes.Buffer
	start := time.Now()
	err := runCommand("sh", []string{"-c", "printf 'started\\n'; sleep 5"}, &output, 2*time.Second, 500*time.Millisecond)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("error = %v, want idle timeout", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("timeout took %s, want under 1s", elapsed)
	}
	if !strings.Contains(output.String(), "started") {
		t.Fatalf("output = %q, want command output", output.String())
	}
}

func TestRunCommandTotalTimeout(t *testing.T) {
	if os.Getenv("CI") == "true" {
		t.Skip("avoid timing-sensitive process test on shared CI")
	}
	var output bytes.Buffer
	err := runCommand("sh", []string{"-c", "i=0; while :; do printf x; i=$((i+1)); sleep 0.01; done"}, &output, 50*time.Millisecond, time.Second)
	if !errors.Is(err, ErrCommandTimeout) {
		t.Fatalf("error = %v, want command timeout", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/tmp/key"); got != "'/tmp/key'" {
		t.Fatalf("shellQuote = %q", got)
	}
	if got := shellQuote("a'b"); got != "'a'\\''b'" {
		t.Fatalf("shellQuote quoted apostrophe = %q", got)
	}
}

func TestValidateRemoteRejectsEmbeddedHTTPSCredentials(t *testing.T) {
	if err := validateRemote("https://token@example.com/repo.git", false); err == nil {
		t.Fatal("validateRemote accepted embedded HTTPS credentials")
	}
	if err := validateRemote("ssh://git@example.com:22/repo.git", true); err != nil {
		t.Fatalf("validateRemote rejected SSH remote: %v", err)
	}
}

func TestDurationSecondsRoundsUp(t *testing.T) {
	if got := durationSeconds(1500 * time.Millisecond); got != 2 {
		t.Fatalf("durationSeconds(1.5s) = %d, want 2", got)
	}
	if got := durationSeconds(0); got != 1 {
		t.Fatalf("durationSeconds(0) = %d, want 1", got)
	}
}

func TestBuildSSHCommandIncludesHostKeyAlias(t *testing.T) {
	command := buildSSHCommand(config{
		Home:                   "/root",
		SSHConnectTimeout:      10 * time.Second,
		SSHServerAliveInterval: 5 * time.Second,
		SSHServerAliveCountMax: 3,
		SSHHostKeyAlias:        "github.com",
	})
	if !strings.Contains(command, "-o HostKeyAlias='github.com'") {
		t.Fatalf("SSH command = %q, want host key alias", command)
	}
}

func TestMirrorCloneReusesCommitWithoutRemoteFetch(t *testing.T) {
	source := t.TempDir()
	runTestGit(t, source, "init", "--initial-branch=main")
	runTestGit(t, source, "config", "user.email", "test@example.com")
	runTestGit(t, source, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("cached clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "README.md")
	runTestGit(t, source, "commit", "-m", "initial")
	commit := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))

	mirror := filepath.Join(t.TempDir(), "mirror.git")
	cfg := config{
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		Commit:          commit,
		Remote:          source,
		Depth:           1,
		Attempts:        1,
		FetchTimeout:    10 * time.Second,
		IdleTimeout:     time.Second,
		ProtocolVersion: "2",
		Home:            t.TempDir(),
		GitBinary:       "git",
		MirrorPath:      mirror,
		MirrorDepth:     5,
	}
	var first bytes.Buffer
	if err := clone(cfg, &first); err != nil {
		t.Fatalf("first clone: %v\n%s", err, first.String())
	}
	if got, err := os.ReadFile(filepath.Join(cfg.Workspace, "README.md")); err != nil || string(got) != "cached clone\n" {
		t.Fatalf("first clone content = %q, err = %v", got, err)
	}
	if err := os.RemoveAll(cfg.Workspace); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := clone(cfg, &second); err != nil {
		t.Fatalf("cached clone: %v\n%s", err, second.String())
	}
	if !strings.Contains(second.String(), "mirror cache hit") {
		t.Fatalf("cached clone output = %q, want mirror cache hit", second.String())
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func TestErrorKind(t *testing.T) {
	if got := errorKind(ErrIdleTimeout); got != "idle-timeout" {
		t.Fatalf("errorKind(idle) = %q, want idle-timeout", got)
	}
	if got := errorKind(errors.Join(errors.New("fetch"), ErrCommandTimeout)); got != "command-timeout" {
		t.Fatalf("errorKind(command) = %q, want command-timeout", got)
	}
}
