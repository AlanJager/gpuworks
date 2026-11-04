package selfinstall

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// The caller really cannot create the atomic temp in parent until this runner
// grants just that fixture directory the write capability sudo would supply.
// Commands still execute as real subprocesses; no host sudo is invoked.
type privilegedWriteRunner struct {
	parent            string
	target            string
	observe           bool
	cancel            context.CancelFunc
	commands          []Command
	modeAtFirstByte   os.FileMode
	dirAtFirstByte    os.FileMode
	targetAtFirstByte []byte
	targetExisted     bool
	observed          bool
	failBin           string
	afterScript       func(Command, error) error
	cleanupContexts   []error
}

func (r *privilegedWriteRunner) run(ctx context.Context, cmd Command) error {
	if err := os.Chmod(r.parent, 0o755); err != nil {
		return err
	}
	if r.failBin != "" && cmd.Path == "sh" {
		cmd.Env = append(cmd.Env, "PATH="+r.failBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// Fix the incoming subprocess umask without changing the test process's
	// umask, so parallel Go tests cannot affect this observation.
	cmd.Args = append([]string{"-c", `umask 022; exec "$@"`, "fixture-umask", cmd.Path}, cmd.Args...)
	cmd.Path, cmd.Sudo = "sh", false
	return (ExecRunner{Euid: func() int { return 0 }}).Run(ctx, cmd)
}

func (r *privilegedWriteRunner) Run(ctx context.Context, cmd Command) error {
	r.commands = append(r.commands, cmd)
	if cmd.Path == "rm" {
		r.cleanupContexts = append(r.cleanupContexts, ctx.Err())
	}
	if !r.observe || (cmd.Path != "tee" && !(cmd.Path == "sh" && len(cmd.Args) > 3 && cmd.Args[2] == "gpuworks-atomic-write")) {
		err := r.run(ctx, cmd)
		if r.afterScript != nil && cmd.Path == "sh" && len(cmd.Args) > 3 && cmd.Args[2] == "gpuworks-atomic-write" {
			return r.afterScript(cmd, err)
		}
		return err
	}
	content, err := io.ReadAll(cmd.Stdin)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return r.run(ctx, cmd)
	}
	observePath, observeDir := cmd.Args[0], r.parent
	if cmd.Path == "sh" {
		observeDir = cmd.Args[3]
		observePath = filepath.Join(observeDir, "content")
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	cmd.Stdin = reader
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.run(ctx, cmd) }()
	if err := waitPrivilegedFile(ctx, observePath, 0); err != nil {
		return err
	}
	if _, err := writer.Write(content[:1]); err != nil {
		return err
	}
	if err := waitPrivilegedFile(ctx, observePath, 1); err != nil {
		return err
	}
	info, err := os.Stat(observePath)
	if err != nil {
		return err
	}
	dir, err := os.Stat(observeDir)
	if err != nil {
		return err
	}
	r.modeAtFirstByte, r.dirAtFirstByte = info.Mode().Perm(), dir.Mode().Perm()
	if targetInfo, statErr := os.Stat(r.target); statErr == nil && targetInfo.IsDir() {
		r.targetExisted = true
	} else {
		r.targetAtFirstByte, err = os.ReadFile(r.target)
		r.targetExisted = err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	r.observed = true
	if r.cancel != nil {
		r.cancel()
		_ = writer.Close()
		return <-done
	}
	if _, err := writer.Write(content[1:]); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	return <-done
}

func (r *privilegedWriteRunner) Output(ctx context.Context, cmd Command) (string, error) {
	r.commands = append(r.commands, cmd)
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := r.run(ctx, cmd); err != nil {
		return "", err
	}
	return strings.TrimSpace(output.String()), nil
}

func waitPrivilegedFile(ctx context.Context, path string, minimum int64) error {
	for {
		info, err := os.Stat(path)
		if err == nil && info.Size() >= minimum {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for staged file: %w", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

func privilegedWriteFixture(t *testing.T, observe bool) (*Installer, *privilegedWriteRunner, string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("requires an ordinary caller to cause the real direct-write permission failure")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	target := filepath.Join(parent, "initial-credentials")
	runner := &privilegedWriteRunner{parent: parent, target: target, observe: observe}
	layout := NewLayout(parent, "", "")
	layout.UnitDir = parent // never inspect a real host service definition
	installer := New(layout, Deps{Run: runner, Euid: func() int { return 1000 }, HasBinary: func(name string) bool { return name == "sudo" }}, nil)
	return installer, runner, target
}

func TestPrivilegedInstalledWriteIsPrivateAtTheFirstByte(t *testing.T) {
	installer, runner, target := privilegedWriteFixture(t, true)
	content := []byte("synthetic bootstrap credential")
	if err := installer.writeInstalled(context.Background(), target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if !runner.observed {
		t.Fatal("real privileged fallback did not write the observed first byte")
	}
	if runner.modeAtFirstByte != 0o600 || runner.dirAtFirstByte != 0o700 {
		t.Errorf("first byte had file=%#o parent=%#o, want0600 inside0700", runner.modeAtFirstByte, runner.dirAtFirstByte)
	}
	if runner.targetExisted {
		t.Error("new target became visible before atomic publish")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("published content mismatch, err=%v", err)
	}
}

func preparePrivilegedTarget(t *testing.T, runner *privilegedWriteRunner, target string, content []byte) {
	t.Helper()
	if err := os.Chmod(runner.parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runner.parent, 0o555); err != nil {
		t.Fatal(err)
	}
}

func assertNoPrivateStages(t *testing.T, parent string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(parent, privilegedStagePrefix+"*"))
	if err != nil || len(stages) != 0 {
		t.Fatalf("private stages remain: %v err=%v", stages, err)
	}
}

func TestPrivilegedInstalledWriteFailuresPreserveTargetAndClean(t *testing.T) {
	for _, fail := range []string{"chmod", "mv", "cancel"} {
		t.Run(fail, func(t *testing.T) {
			installer, runner, target := privilegedWriteFixture(t, fail == "cancel")
			old := []byte("previous private credential")
			preparePrivilegedTarget(t, runner, target, old)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if fail == "cancel" {
				runner.cancel = cancel
			} else {
				runner.failBin = t.TempDir()
				if err := os.WriteFile(filepath.Join(runner.failBin, fail), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := installer.writeInstalled(ctx, target, []byte("synthetic replacement credential"), 0o600); err == nil {
				t.Fatal("injected failure succeeded")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, old) {
				t.Fatalf("failed write replaced old target, err=%v", err)
			}
			info, err := os.Stat(target)
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatalf("old target mode changed: %v", err)
			}
			assertNoPrivateStages(t, runner.parent)
			if fail == "cancel" {
				if !runner.observed || runner.modeAtFirstByte != 0o600 || runner.dirAtFirstByte != 0o700 {
					t.Fatal("cancellation was not after a real private first byte")
				}
				if !bytes.Equal(runner.targetAtFirstByte, old) {
					t.Fatal("target changed before publishing")
				}
				if len(runner.cleanupContexts) == 0 || runner.cleanupContexts[0] != nil {
					t.Fatal("cleanup inherited canceled context")
				}
			}
		})
	}
}

func TestPrivilegedInstalledWriteReplacesSymlinkWithoutFollowing(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint("directory=", directory), func(t *testing.T) {
			installer, runner, target := privilegedWriteFixture(t, true)
			victimRoot := t.TempDir()
			victim := filepath.Join(victimRoot, "victim")
			if directory {
				if err := os.Mkdir(victim, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(runner.parent, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(runner.parent, 0o555); err != nil {
				t.Fatal(err)
			}
			content := []byte("synthetic private replacement")
			if err := installer.writeInstalled(context.Background(), target, content, 0o600); err != nil {
				t.Fatal(err)
			}
			info, err := os.Lstat(target)
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
				t.Fatalf("target was not atomically replaced: %v", err)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatal("replacement content wrong")
			}
			if directory {
				entries, err := os.ReadDir(victim)
				if err != nil || len(entries) != 0 {
					t.Fatalf("symlink directory victim changed: %v", err)
				}
			} else {
				got, err := os.ReadFile(victim)
				if err != nil || string(got) != "untouched" {
					t.Fatal("symlink file victim changed")
				}
			}
			assertNoPrivateStages(t, runner.parent)
		})
	}
}

func TestPrivilegedInstalledCopyDoesNotUsePredictableStage(t *testing.T) {
	installer, runner, target := privilegedWriteFixture(t, false)
	src := filepath.Join(t.TempDir(), "source")
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(src, []byte("synthetic binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runner.parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, target+".sudo-tmp"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(runner.parent, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := installer.copyInstalled(context.Background(), src, target, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(victim)
	if err != nil || string(got) != "untouched" {
		t.Fatal("predictable-stage symlink victim was overwritten")
	}
	got, err = os.ReadFile(target)
	if err != nil || string(got) != "synthetic binary" {
		t.Fatal("binary copy wrong")
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatal("binary final mode wrong")
	}
	assertNoPrivateStages(t, runner.parent)
}

func TestPrivilegedWriterShortInputPreservesTarget(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("reader-error=", readFailure), func(t *testing.T) {
			installer, runner, target := privilegedWriteFixture(t, false)
			old := []byte("old credential")
			preparePrivilegedTarget(t, runner, target, old)
			var reader io.Reader = strings.NewReader("short")
			if readFailure {
				reader = io.MultiReader(reader, iotest.ErrReader(errors.New("injected input error")))
			}
			if err := installer.writePrivileged(context.Background(), target, reader, 100, 0o600); err == nil {
				t.Fatal("short input was published")
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, old) {
				t.Fatal("short input changed target")
			}
			assertNoPrivateStages(t, runner.parent)
		})
	}
}

func TestPrivilegedWriterRejectsUntrustedParentChain(t *testing.T) {
	for _, kind := range []string{"world-writable parent", "writable descendant below sticky ancestor"} {
		t.Run(kind, func(t *testing.T) {
			installer, runner, target := privilegedWriteFixture(t, false)
			if kind == "world-writable parent" {
				if err := os.Chmod(runner.parent, 0o777); err != nil {
					t.Fatal(err)
				}
			} else {
				if os.Getgid() == 0 {
					t.Skip("needs a group outside the trusted root group")
				}
				if err := os.Chmod(runner.parent, 0o777|os.ModeSticky); err != nil {
					t.Fatal(err)
				}
				middle := filepath.Join(runner.parent, "untrusted")
				if err := os.Mkdir(middle, 0o775); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(middle, 0o775); err != nil {
					t.Fatal(err)
				}
				parent := filepath.Join(middle, "private")
				if err := os.Mkdir(parent, 0o555); err != nil {
					t.Fatal(err)
				}
				runner.parent, target = parent, filepath.Join(parent, "initial-credentials")
			}
			if err := installer.writePrivileged(context.Background(), target, strings.NewReader("synthetic credential"), 20, 0o600); err == nil {
				t.Fatal("unsafe parent accepted")
			}
			if len(runner.commands) != 0 {
				t.Fatal("unsafe parent reached privileged command execution")
			}
			if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("unsafe target written: %v", err)
			}
		})
	}
}

func TestPrivilegedWriterKeepsReplacedStageAndPrimaryError(t *testing.T) {
	installer, runner, target := privilegedWriteFixture(t, false)
	old := []byte("old credential")
	preparePrivilegedTarget(t, runner, target, old)
	runner.failBin = t.TempDir()
	if err := os.WriteFile(filepath.Join(runner.failBin, "chmod"), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	sentinel := filepath.Join(victim, "keep")
	if err := os.WriteFile(sentinel, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	primary := errors.New("primary execution failure")
	runner.afterScript = func(cmd Command, err error) error {
		if err == nil {
			return errors.New("injected chmod failure did not execute")
		}
		stage := cmd.Args[3]
		if renameErr := os.Rename(stage, stage+"-original"); renameErr != nil {
			return renameErr
		}
		if linkErr := os.Symlink(victim, stage); linkErr != nil {
			return linkErr
		}
		return errors.Join(primary, err)
	}
	err := installer.writeInstalled(context.Background(), target, []byte("synthetic replacement"), 0o600)
	if !errors.Is(err, primary) || !strings.Contains(err.Error(), "refusing cleanup") {
		t.Fatalf("cleanup masked primary error: %v", err)
	}
	got, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(got) != "untouched" {
		t.Fatal("cleanup followed a replacement staging symlink")
	}
	for _, cmd := range runner.commands {
		if cmd.Path == "rm" {
			t.Fatal("cleanup attempted deletion after losing stage identity")
		}
	}
}

func TestPrivilegedWriterTreatsPathsAsDataAndReplacesExisting(t *testing.T) {
	installer, runner, _ := privilegedWriteFixture(t, true)
	target := filepath.Join(runner.parent, `credential ' $(printf injected) ;`)
	runner.target = target
	old, content := []byte("old credential"), []byte("synthetic new credential")
	preparePrivilegedTarget(t, runner, target, old)
	if err := installer.writeInstalled(context.Background(), target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(runner.targetAtFirstByte, old) {
		t.Fatal("old target changed before atomic publication")
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatal("literal path was evaluated or changed")
	}
	for _, cmd := range runner.commands {
		for _, arg := range cmd.Args {
			if strings.Contains(arg, string(content)) {
				t.Fatal("credential appeared in a command argument")
			}
		}
	}
	assertNoPrivateStages(t, runner.parent)
}
