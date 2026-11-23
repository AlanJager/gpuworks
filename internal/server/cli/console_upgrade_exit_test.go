package cli

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/selfinstall"
)

// The server's log is written by its own goroutine and the deferred closer
// does not run past os.Exit: the line that says why the server exits, and so
// why the next start rolls the upgrade back, must be in the log file by the
// time the process ends, along with what was logged before it.
func TestTheReasonForExitingAfterAFailedConsoleUpgradeIsInTheLogFile(t *testing.T) {
	// A loopback address nothing listens on.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	loopback := "http://" + listener.Addr().String()
	listener.Close()

	dir := t.TempDir()
	logPath := filepath.Join(dir, "logs", "server.log")
	log, closeLog := serverLogger(logPath, "10MB", 2, slog.LevelInfo)
	defer closeLog()

	savedTimeout, savedExit := consoleUpgradeConfirmTimeout, exitProcess
	consoleUpgradeConfirmTimeout = 0
	t.Cleanup(func() { consoleUpgradeConfirmTimeout, exitProcess = savedTimeout, savedExit })
	exited, atExit := 0, ""
	exitProcess = func(code int) {
		exited = code
		data, _ := os.ReadFile(logPath)
		atExit = string(data)
	}

	for i := 0; i < 200; i++ {
		log.Info("what the server did before it gave up", "n", i)
	}
	layout := selfinstall.NewLayout(filepath.Join(dir, "config"), filepath.Join(dir, "data"), "")
	confirmConsoleUpgrade(context.Background(), log, layout, selfinstall.PendingUpgrade{From: "1.0.0", To: "1.0.1"}, loopback)

	if exited != 1 {
		t.Fatalf("exit code %d, want 1", exited)
	}
	if !strings.Contains(atExit, "console upgrade did not become healthy; exiting so the next start rolls back") {
		t.Fatalf("the log file lacked the reason when the process exited:\n%s", atExit)
	}
	if got := strings.Count(atExit, "what the server did before it gave up"); got != 200 {
		t.Fatalf("the log file had %d of the 200 records before the exit", got)
	}
}
