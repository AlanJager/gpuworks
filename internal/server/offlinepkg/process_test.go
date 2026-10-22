package offlinepkg

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// packageOutput waits for the installer and its background fixture agents.
// Their stdio is redirected, so CombinedOutput alone cannot track their lifetime.
// The inherited pipe closes only after every descendant has exited; no PID scan
// or signal to other processes is needed before reading calls or removing files.
func packageOutput(cmd *exec.Cmd) ([]byte, error) {
	finished, inherited, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer finished.Close()
	defer inherited.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, inherited)
	out, commandErr := cmd.CombinedOutput()
	inherited.Close()
	if err := finished.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return out, errors.Join(commandErr, err)
	}
	if _, err := io.Copy(io.Discard, finished); err != nil {
		return out, errors.Join(commandErr, fmt.Errorf("wait for package fixture descendants: %w", err))
	}
	return out, commandErr
}

func TestPackageOutputWaitsForBackgroundAgent(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	releaseRead, releaseWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead.Close()
	defer releaseWrite.Close()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	completed := filepath.Join(t.TempDir(), "completed")
	// Like install.sh's nohup branch, the installer exits with its agent's
	// output redirected. Hold that agent until the test explicitly releases it.
	cmd := exec.Command("sh", "-c", `(printf r >&4; IFS= read -r release <&3 || exit 1; printf completed > "$COMPLETED") >/dev/null 2>&1 &`)
	cmd.Env = []string{"COMPLETED=" + completed}
	cmd.ExtraFiles = []*os.File{releaseRead, readyWrite}
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := packageOutput(cmd)
		done <- result{out, err}
	}()
	// Even on assertion failure release this test's child before TempDir cleanup.
	defer func() {
		releaseWrite.Close()
		readyWrite.Close()
		if _, err := io.Copy(io.Discard, readyRead); err != nil {
			t.Errorf("wait for test background agent: %v", err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("background command did not finish")
		}
	}()
	if err := readyRead.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if n, err := readyRead.Read(make([]byte, 1)); err != nil || n != 1 {
		t.Fatalf("background agent did not become ready: %d, %v", n, err)
	}
	select {
	case result := <-done:
		done <- result
		t.Fatalf("package returned while its background agent could still write: %v, %s", result.err, result.out)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := releaseWrite.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		done <- result
		if result.err != nil {
			t.Fatalf("package: %v, %s", result.err, result.out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("package did not finish after releasing its background agent")
	}
	if data, err := os.ReadFile(completed); err != nil || string(data) != "completed" {
		t.Fatalf("agent completion = %q, %v", data, err)
	}
}
