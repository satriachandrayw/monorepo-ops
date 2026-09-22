package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	ErrIdleTimeout    = errors.New("command idle timeout")
	ErrCommandTimeout = errors.New("command timeout")
)

type config struct {
	Workspace              string
	Commit                 string
	Branch                 string
	Remote                 string
	RemoteSSH              string
	UseSSH                 bool
	Depth                  int
	Tags                   bool
	Attempts               int
	Backoff                time.Duration
	FetchTimeout           time.Duration
	IdleTimeout            time.Duration
	ProtocolVersion        string
	SSHConnectTimeout      time.Duration
	SSHServerAliveInterval time.Duration
	SSHServerAliveCountMax int
	SSHKeyPrivate          string
	SSHHostKey             string
	SSHHostKeyAlias        string
	NetrcMachine           string
	NetrcUsername          string
	NetrcPassword          string
	Home                   string
	GitBinary              string
	MirrorPath             string
	MirrorDepth            int
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resilient-git: %v\n", err)
		os.Exit(1)
	}
	if err := clone(cfg, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "resilient-git: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig() (config, error) {
	cfg := config{
		Workspace:              envOr("CI_WORKSPACE", "/woodpecker/src"),
		Commit:                 os.Getenv("CI_COMMIT_SHA"),
		Branch:                 os.Getenv("CI_COMMIT_BRANCH"),
		Remote:                 envOr("PLUGIN_REMOTE", os.Getenv("CI_REPO_CLONE_URL")),
		RemoteSSH:              envOr("PLUGIN_REMOTE_SSH", os.Getenv("CI_REPO_CLONE_SSH_URL")),
		UseSSH:                 envBool("PLUGIN_USE_SSH", false),
		Depth:                  envInt("PLUGIN_DEPTH", 1),
		Tags:                   envBool("PLUGIN_TAGS", false),
		Attempts:               envInt("PLUGIN_ATTEMPTS", 3),
		Backoff:                envDuration("PLUGIN_BACKOFF", 5*time.Second),
		FetchTimeout:           envDuration("PLUGIN_FETCH_TIMEOUT", 10*time.Minute),
		IdleTimeout:            envDuration("PLUGIN_IDLE_TIMEOUT", 90*time.Second),
		ProtocolVersion:        envOr("PLUGIN_PROTOCOL_VERSION", "0"),
		SSHConnectTimeout:      envDuration("PLUGIN_SSH_CONNECT_TIMEOUT", 10*time.Second),
		SSHServerAliveInterval: envDuration("PLUGIN_SSH_SERVER_ALIVE_INTERVAL", 5*time.Second),
		SSHServerAliveCountMax: envInt("PLUGIN_SSH_SERVER_ALIVE_COUNT_MAX", 3),
		SSHKeyPrivate:          os.Getenv("PLUGIN_SSH_KEY_PRIVATE"),
		SSHHostKey:             os.Getenv("PLUGIN_SSH_HOST_KEY"),
		SSHHostKeyAlias:        os.Getenv("PLUGIN_SSH_HOST_KEY_ALIAS"),
		NetrcMachine:           os.Getenv("CI_NETRC_MACHINE"),
		NetrcUsername:          os.Getenv("CI_NETRC_USERNAME"),
		NetrcPassword:          os.Getenv("CI_NETRC_PASSWORD"),
		Home:                   envOr("HOME", "/root"),
		GitBinary:              envOr("PLUGIN_GIT_BINARY", "git"),
		MirrorPath:             os.Getenv("PLUGIN_MIRROR_PATH"),
		MirrorDepth:            envInt("PLUGIN_MIRROR_DEPTH", 5),
	}
	if cfg.UseSSH {
		cfg.Remote = cfg.RemoteSSH
	}
	if cfg.Workspace == "" {
		return config{}, errors.New("CI_WORKSPACE is required")
	}
	if cfg.Commit == "" {
		return config{}, errors.New("CI_COMMIT_SHA is required")
	}
	if cfg.Remote == "" {
		return config{}, errors.New("clone remote is required")
	}
	if err := validateRemote(cfg.Remote, cfg.UseSSH); err != nil {
		return config{}, err
	}
	if cfg.UseSSH && cfg.SSHKeyPrivate == "" {
		return config{}, errors.New("PLUGIN_SSH_KEY_PRIVATE is required for SSH clone")
	}
	if cfg.UseSSH && cfg.SSHHostKey == "" {
		return config{}, errors.New("PLUGIN_SSH_HOST_KEY is required for SSH clone")
	}
	if cfg.Depth < 0 || cfg.Attempts < 1 || cfg.FetchTimeout <= 0 || cfg.IdleTimeout <= 0 ||
		cfg.SSHConnectTimeout <= 0 || cfg.SSHServerAliveInterval <= 0 || cfg.SSHServerAliveCountMax < 1 ||
		(cfg.MirrorPath != "" && (!filepath.IsAbs(cfg.MirrorPath) || cfg.MirrorDepth < 1)) {
		return config{}, errors.New("invalid clone timeout, depth, attempts, or mirror setting")
	}
	return cfg, nil
}

func clone(cfg config, output io.Writer) error {
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	if err := prepareCredentials(cfg); err != nil {
		return err
	}

	return retry(cfg.Attempts, cfg.Backoff, func(attempt int) error {
		if attempt > 1 {
			fmt.Fprintf(output, "resilient-git: retrying fetch (attempt %d/%d)\n", attempt, cfg.Attempts)
		}
		fmt.Fprintf(output, "resilient-git: fetch attempt %d/%d started (idle timeout %s, total timeout %s)\n", attempt, cfg.Attempts, cfg.IdleTimeout, cfg.FetchTimeout)
		if cfg.MirrorPath != "" {
			if err := ensureMirrorCommit(cfg, output); err != nil {
				reportAttemptFailure(output, attempt, cfg.Attempts, err)
				return err
			}
			if err := initializeFromMirror(cfg, output); err != nil {
				reportAttemptFailure(output, attempt, cfg.Attempts, err)
				return err
			}
		} else {
			if err := initializeRepository(cfg, output); err != nil {
				reportAttemptFailure(output, attempt, cfg.Attempts, err)
				return err
			}
			if err := fetchCommit(cfg, output); err != nil {
				_ = os.RemoveAll(filepath.Join(cfg.Workspace, ".git"))
				reportAttemptFailure(output, attempt, cfg.Attempts, err)
				return err
			}
		}
		if err := runGit(cfg, output, "-C", cfg.Workspace, "reset", "--hard", "-q", cfg.Commit); err != nil {
			_ = os.RemoveAll(filepath.Join(cfg.Workspace, ".git"))
			err = fmt.Errorf("checkout %s: %w", cfg.Commit, err)
			reportAttemptFailure(output, attempt, cfg.Attempts, err)
			return err
		}
		return nil
	})
}

func prepareCredentials(cfg config) error {
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return fmt.Errorf("create git home: %w", err)
	}
	if cfg.NetrcMachine != "" && cfg.NetrcUsername != "" && cfg.NetrcPassword != "" {
		contents := fmt.Sprintf("machine %s login %s password %s\n", cfg.NetrcMachine, cfg.NetrcUsername, cfg.NetrcPassword)
		if err := os.WriteFile(filepath.Join(cfg.Home, ".netrc"), []byte(contents), 0o600); err != nil {
			return fmt.Errorf("write git credentials: %w", err)
		}
	}
	if !cfg.UseSSH {
		return nil
	}
	keyPath := filepath.Join(cfg.Home, "sshkey")
	if err := os.WriteFile(keyPath, []byte(cfg.SSHKeyPrivate), 0o600); err != nil {
		return fmt.Errorf("write SSH key: %w", err)
	}
	sshDir := filepath.Join(cfg.Home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return fmt.Errorf("create SSH directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(cfg.SSHHostKey), 0o600); err != nil {
		return fmt.Errorf("write SSH host key: %w", err)
	}
	return nil
}

func ensureMirrorCommit(cfg config, output io.Writer) error {
	if err := os.MkdirAll(filepath.Dir(cfg.MirrorPath), 0o755); err != nil {
		return fmt.Errorf("create mirror parent: %w", err)
	}
	lock, err := os.OpenFile(cfg.MirrorPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open mirror lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock mirror: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if _, err := os.Stat(filepath.Join(cfg.MirrorPath, "config")); os.IsNotExist(err) {
		if err := runGit(cfg, output, "init", "--bare", "--object-format", "sha1", cfg.MirrorPath); err != nil {
			return fmt.Errorf("initialize mirror: %w", err)
		}
	}
	if err := runGit(cfg, io.Discard, "-C", cfg.MirrorPath, "remote", "set-url", "origin", cfg.Remote); err != nil {
		if err := runGit(cfg, output, "-C", cfg.MirrorPath, "remote", "add", "origin", cfg.Remote); err != nil {
			return fmt.Errorf("configure mirror origin: %w", err)
		}
	}
	if cfg.UseSSH {
		if err := runGit(cfg, output, "-C", cfg.MirrorPath, "config", "core.sshCommand", buildSSHCommand(cfg)); err != nil {
			return fmt.Errorf("configure mirror SSH: %w", err)
		}
	}
	if err := configureLocalRemoteSafety(cfg); err != nil {
		return err
	}
	if err := removeStaleMirrorLocks(cfg.MirrorPath, output); err != nil {
		return err
	}
	if mirrorHasCommit(cfg) {
		fmt.Fprintf(output, "resilient-git: mirror cache hit for %s\n", cfg.Commit)
		return nil
	}

	args := []string{"-C", cfg.MirrorPath}
	if cfg.ProtocolVersion != "" {
		args = append(args, "-c", "protocol.version="+cfg.ProtocolVersion)
	}
	args = append(args, "fetch", "--no-tags", "--depth", strconv.Itoa(cfg.MirrorDepth), "origin", "+"+cfg.Commit+":refs/heads/woodpecker-cache")
	if err := runGit(cfg, output, args...); err != nil {
		return fmt.Errorf("update mirror for %s: %w", cfg.Commit, err)
	}
	if err := runGit(cfg, output, "-C", cfg.MirrorPath, "symbolic-ref", "HEAD", "refs/heads/woodpecker-cache"); err != nil {
		return fmt.Errorf("set mirror HEAD: %w", err)
	}
	if !mirrorHasCommit(cfg) {
		return fmt.Errorf("mirror does not contain %s after fetch", cfg.Commit)
	}
	fmt.Fprintf(output, "resilient-git: mirror updated for %s\n", cfg.Commit)
	return nil
}

func removeStaleMirrorLocks(mirrorPath string, output io.Writer) error {
	return filepath.WalkDir(mirrorPath, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("inspect mirror lock state: %w", err)
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".lock") {
			return nil
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale mirror lock %s: %w", path, err)
		}
		fmt.Fprintf(output, "resilient-git: removed stale mirror lock %s\n", path)
		return nil
	})
}

func configureLocalRemoteSafety(cfg config) error {
	if cfg.UseSSH {
		return nil
	}
	parsed, err := url.Parse(cfg.Remote)
	if err != nil || (parsed.Scheme != "" && parsed.Scheme != "file") {
		return nil
	}
	path := cfg.Remote
	if parsed.Scheme == "file" {
		path = parsed.Path
	}
	if path == "" {
		return nil
	}
	if err := runGit(cfg, io.Discard, "config", "--global", "--add", "safe.directory", path); err != nil {
		return fmt.Errorf("configure local remote safety: %w", err)
	}
	return nil
}

func mirrorHasCommit(cfg config) bool {
	return runGit(cfg, io.Discard, "-C", cfg.MirrorPath, "cat-file", "-e", cfg.Commit+"^{commit}") == nil
}

func cleanWorkspace(workspace string) error {
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(workspace, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func initializeFromMirror(cfg config, output io.Writer) error {
	if err := cleanWorkspace(cfg.Workspace); err != nil {
		return fmt.Errorf("clean workspace: %w", err)
	}
	if err := os.MkdirAll(cfg.Workspace, 0o755); err != nil {
		return fmt.Errorf("create workspace: %w", err)
	}
	if err := runGit(cfg, output, "clone", "--no-hardlinks", "--no-checkout", cfg.MirrorPath, cfg.Workspace); err != nil {
		return fmt.Errorf("clone from mirror: %w", err)
	}
	if err := runGit(cfg, output, "-C", cfg.Workspace, "config", "--global", "--replace-all", "safe.directory", cfg.Workspace); err != nil {
		return fmt.Errorf("configure safe directory: %w", err)
	}
	if err := runGit(cfg, output, "-C", cfg.Workspace, "remote", "set-url", "origin", cfg.Remote); err != nil {
		return fmt.Errorf("configure origin: %w", err)
	}
	if cfg.UseSSH {
		if err := runGit(cfg, output, "-C", cfg.Workspace, "config", "--global", "core.sshCommand", buildSSHCommand(cfg)); err != nil {
			return fmt.Errorf("configure SSH: %w", err)
		}
	}
	return nil
}

func initializeRepository(cfg config, output io.Writer) error {
	if err := os.RemoveAll(filepath.Join(cfg.Workspace, ".git")); err != nil {
		return fmt.Errorf("clean previous repository: %w", err)
	}
	args := []string{"init", "--object-format", "sha1"}
	if cfg.Branch != "" {
		args = append(args, "-b", cfg.Branch)
	}
	args = append(args, cfg.Workspace)
	if err := runGit(cfg, output, args...); err != nil {
		return fmt.Errorf("initialize repository: %w", err)
	}
	if err := runGit(cfg, output, "-C", cfg.Workspace, "config", "--global", "--replace-all", "safe.directory", cfg.Workspace); err != nil {
		return fmt.Errorf("configure safe directory: %w", err)
	}
	remote := cfg.Remote
	if err := runGit(cfg, output, "-C", cfg.Workspace, "remote", "add", "origin", remote); err != nil {
		return fmt.Errorf("configure origin: %w", err)
	}
	if cfg.UseSSH {
		sshCommand := buildSSHCommand(cfg)
		if err := runGit(cfg, output, "-C", cfg.Workspace, "config", "--global", "core.sshCommand", sshCommand); err != nil {
			return fmt.Errorf("configure SSH: %w", err)
		}
	}
	return nil
}

func fetchCommit(cfg config, output io.Writer) error {
	args := []string{"-C", cfg.Workspace}
	if cfg.ProtocolVersion != "" {
		args = append(args, "-c", "protocol.version="+cfg.ProtocolVersion)
	}
	args = append(args, "fetch")
	if cfg.Tags {
		args = append(args, "--tags")
	} else {
		args = append(args, "--no-tags")
	}
	if cfg.Depth > 0 {
		args = append(args, "--depth", strconv.Itoa(cfg.Depth))
	}
	args = append(args, "origin", "+"+cfg.Commit+":")
	if err := runGit(cfg, output, args...); err != nil {
		return fmt.Errorf("fetch %s: %w", cfg.Commit, err)
	}
	return nil
}

func runGit(cfg config, output io.Writer, args ...string) error {
	return runCommandWithTimeout(cfg.GitBinary, args, output, cfg.FetchTimeout, cfg.IdleTimeout)
}

func retry(attempts int, backoff time.Duration, fn func(int) error) error {
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := fn(attempt); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < attempts {
			delay := backoff * time.Duration(1<<(attempt-1))
			time.Sleep(delay)
		}
	}
	return fmt.Errorf("all %d fetch attempts failed: %w", attempts, lastErr)
}

func reportAttemptFailure(output io.Writer, attempt, attempts int, err error) {
	message := strings.Join(strings.Fields(err.Error()), " ")
	fmt.Fprintf(output, "resilient-git: attempt %d/%d failed (%s): %s\n", attempt, attempts, errorKind(err), message)
}

func errorKind(err error) string {
	switch {
	case errors.Is(err, ErrIdleTimeout):
		return "idle-timeout"
	case errors.Is(err, ErrCommandTimeout):
		return "command-timeout"
	default:
		return "git-error"
	}
}

func buildSSHCommand(cfg config) string {
	sshCommand := "ssh -i " + shellQuote(filepath.Join(cfg.Home, "sshkey")) +
		" -o UserKnownHostsFile=" + shellQuote(filepath.Join(cfg.Home, ".ssh", "known_hosts")) +
		" -o IdentitiesOnly=yes -o BatchMode=yes -o ConnectionAttempts=1" +
		" -o ConnectTimeout=" + strconv.Itoa(durationSeconds(cfg.SSHConnectTimeout)) +
		" -o ServerAliveInterval=" + strconv.Itoa(durationSeconds(cfg.SSHServerAliveInterval)) +
		" -o ServerAliveCountMax=" + strconv.Itoa(cfg.SSHServerAliveCountMax)
	if cfg.SSHHostKeyAlias != "" {
		sshCommand += " -o HostKeyAlias=" + shellQuote(cfg.SSHHostKeyAlias)
	}
	return sshCommand
}

func durationSeconds(value time.Duration) int {
	seconds := int(value / time.Second)
	if value%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}

func runCommand(name string, args []string, output io.Writer, totalTimeout, idleTimeout time.Duration) error {
	return runCommandWithTimeout(name, args, output, totalTimeout, idleTimeout)
}

func runCommandWithTimeout(name string, args []string, output io.Writer, totalTimeout, idleTimeout time.Duration) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = sanitizedEnvironment()
	activity := make(chan struct{}, 16)
	writer := &activityWriter{dst: output, activity: activity}
	cmd.Stdout = writer
	cmd.Stderr = writer
	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	total := time.NewTimer(totalTimeout)
	defer total.Stop()
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(idleTimeout)
	}
	kill := func() {
		if cmd.Process == nil {
			return
		}
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
	}

	for {
		select {
		case err := <-done:
			return err
		case <-activity:
			resetIdle()
		case <-idle.C:
			kill()
			<-done
			return fmt.Errorf("%w after %s", ErrIdleTimeout, idleTimeout)
		case <-total.C:
			kill()
			<-done
			return fmt.Errorf("%w after %s", ErrCommandTimeout, totalTimeout)
		}
	}
}

func sanitizedEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+3)
	for _, entry := range os.Environ() {
		key := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			key = entry[:index]
		}
		if key == "GIT_TRACE" || key == "GIT_TRACE_PACKET" || key == "GIT_TRACE_PERFORMANCE" || key == "GIT_CURL_VERBOSE" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "GCM_INTERACTIVE=Never")
}

type activityWriter struct {
	dst      io.Writer
	activity chan<- struct{}
	mu       sync.Mutex
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.dst.Write(p)
	if n > 0 {
		select {
		case w.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func validateRemote(remote string, useSSH bool) error {
	parsed, err := url.Parse(remote)
	if err != nil {
		return fmt.Errorf("invalid clone remote: %w", err)
	}
	if !useSSH && parsed.User != nil {
		return errors.New("HTTPS clone remote must not contain embedded credentials")
	}
	return nil
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return value
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return time.Duration(seconds) * time.Second
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
