package gpu

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func shCmd(script string) func(context.Context) *exec.Cmd {
	return func(ctx context.Context) *exec.Cmd { return exec.CommandContext(ctx, "sh", "-c", script) }
}

// Calls under one key take turns: two callers arriving together never have
// two processes on the driver at once.
func TestRunTrackedCallsUnderOneKeyTakeTurns(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "running")
	script := `if [ -e "` + marker + `" ]; then echo overlap; exit 1; fi; touch "` + marker + `"; sleep 0.2; rm "` + marker + `"; echo ok`
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := RunTracked(t.Context(), "gw-test-turns", 5*time.Second, shCmd(script))
			if err != nil || strings.TrimSpace(out) != "ok" {
				errs <- errors.New(out)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("calls overlapped: %v", err)
	}
}

// Once a call under a key is stuck past its kill, every other command under
// the same key is refused without starting, while other keys run on.
func TestRunTrackedStuckKeyRefusesEveryCommandUnderIt(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is needed to keep a process out of the killed group")
	}
	shortVendorCLITimeouts(t, time.Second, 50*time.Millisecond)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	// The child in its own session holds the output pipe past the group
	// kill, standing in for a process stuck in the driver.
	_, err := RunTracked(t.Context(), "gw-test-stuck", 100*time.Millisecond, shCmd("setsid sleep 1.5 & sleep 5"))
	if !IsCLIUnresponsive(err) {
		t.Fatalf("first call err = %v", err)
	}
	_, err = RunTracked(t.Context(), "gw-test-stuck", time.Second, shCmd(`touch "`+started+`"`))
	if !errors.Is(err, ErrStillRunning) {
		t.Fatalf("second command err = %v, want refused", err)
	}
	if _, statErr := os.Stat(started); statErr == nil {
		t.Fatal("a command was started under a stuck key")
	}
	if out, err := RunTracked(t.Context(), "gw-test-other", time.Second, shCmd("echo free")); err != nil || strings.TrimSpace(out) != "free" {
		t.Fatalf("another key = %q %v", out, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := RunTracked(t.Context(), "gw-test-stuck", time.Second, shCmd("true"))
		if err == nil {
			break
		}
		if !errors.Is(err, ErrStillRunning) || time.Now().After(deadline) {
			t.Fatalf("key never recovered: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A runtime check refused because an earlier one is stuck is the host's
// answer, never the card's, for every vendor's verdict.
func TestARefusedRuntimeCheckIsNotTheCards(t *testing.T) {
	refused := errors.Join(ErrStillRunning)
	for name, verdict := range map[string]error{
		"cuda":   cudaSmokeVerdict(nil, refused),
		"ascend": ascendSmokeVerdict(nil, refused),
		"hygon":  hygonSmokeVerdict(nil, refused),
	} {
		if HostRuntimeUnavailable(verdict) == "" {
			t.Errorf("%s: %v, want host-level", name, verdict)
		}
	}
}

func TestHCCNKeyIsTheDie(t *testing.T) {
	for args, want := range map[string]string{
		"-i 3 -ip -g":             "hccn_tool -i 3",
		"-i 3 -ping -g address 1": "hccn_tool -i 3",
		"-h":                      "hccn_tool",
	} {
		if got := hccnKey(strings.Fields(args)); got != want {
			t.Errorf("hccnKey(%q) = %q, want %q", args, got, want)
		}
	}
}
