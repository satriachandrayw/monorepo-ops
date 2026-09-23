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
	firstCommit := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))

	mirror := filepath.Join(t.TempDir(), "mirror.git")
	cfg := config{
		Workspace:       filepath.Join(t.TempDir(), "workspace"),
		Commit:          firstCommit,
		Remote:          source,
		Depth:           1,
		Attempts:        1,
		FetchTimeout:    10 * time.Second,
		IdleTimeout:     time.Second,
		ProtocolVersion: "2",
		Home:            t.TempDir(),
		GitBinary:       "git",
		MirrorPath:      mirror,
		MirrorDepth:     1,
	}
	var first bytes.Buffer
	if err := clone(cfg, &first); err != nil {
		t.Fatalf("first clone: %v\n%s", err, first.String())
	}
	assertWorkspaceContent(t, cfg.Workspace, "cached clone\n")

	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("newer clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, source, "add", "README.md")
	runTestGit(t, source, "commit", "-m", "second")
	secondCommit := strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD"))
	cfg.Commit = secondCommit
	var newer bytes.Buffer
	if err := clone(cfg, &newer); err != nil {
		t.Fatalf("newer clone: %v\n%s", err, newer.String())
	}
	assertWorkspaceContent(t, cfg.Workspace, "newer clone\n")

	// Re-request an older commit that remains in the mirror after its shallow
	// HEAD advanced. The workspace clone must expose the requested commit, not
	// just report a cache hit for an object it cannot copy from the current HEAD.
	cfg.Commit = firstCommit
	var cached bytes.Buffer
	if err := clone(cfg, &cached); err != nil {
		t.Fatalf("cached older clone: %v\n%s", err, cached.String())
	}
	if !strings.Contains(cached.String(), "mirror cache hit") {
		t.Fatalf("cached clone output = %q, want mirror cache hit", cached.String())
	}
	assertWorkspaceContent(t, cfg.Workspace, "cached clone\n")
}

func assertWorkspaceContent(t *testing.T, workspace, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(workspace, "README.md"))
	if err != nil || string(got) != want {
		t.Fatalf("workspace README = %q, want %q, err = %v", got, want, err)
	}
}

func TestRemoveStaleMirrorLocks(t *testing.T) {
	mirror := t.TempDir()
	staleRootLock := filepath.Join(mirror, "shallow.lock")
	staleRefLock := filepath.Join(mirror, "refs", "heads", "woodpecker-cache.lock")
	if err := os.MkdirAll(filepath.Dir(staleRefLock), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{staleRootLock, staleRefLock} {
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(mirror, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := removeStaleMirrorLocks(mirror, &output); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{staleRootLock, staleRefLock} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale lock %s still exists, stat error = %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mirror, "keep.txt")); err != nil {
		t.Fatalf("non-lock file was removed: %v", err)
	}
	if got := strings.Count(output.String(), "removed stale mirror lock"); got != 2 {
		t.Fatalf("cleanup output count = %d, want 2: %q", got, output.String())
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
