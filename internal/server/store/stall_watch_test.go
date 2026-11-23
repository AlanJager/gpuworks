package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// A statement that does not come back is what a stalled disk or database
// process looks like from the store. The process's stall watch must name it
// and keep the stack of the goroutine that waits in it.
func TestAStatementThatDoesNotReturnIsInAStallReport(t *testing.T) {
	st := newTestStore(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	if err := st.db.Callback().Query().Before("gorm:query").Register("test:block", func(tx *gorm.DB) {
		if tx.Statement.Table == "nodes" {
			entered <- struct{}{}
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "stalls")
	w := stallwatch.New(stallwatch.Options{Threshold: 100 * time.Millisecond, Dir: dir})
	stallwatch.Use(w)
	t.Cleanup(func() { stallwatch.Use(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.Run(ctx) }()

	// Statements that return in time leave nothing.
	var count int64
	if err := st.db.Table("users").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("SELECT 1").Error; err != nil {
		t.Fatal(err)
	}

	listed := make(chan error, 1)
	go func() {
		var nodes []Node
		listed <- st.db.Find(&nodes).Error
	}()
	<-entered
	report := ""
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(report, "goroutines:") {
		if time.Now().After(deadline) {
			t.Fatal("no stall report for a statement that does not return")
		}
		time.Sleep(10 * time.Millisecond)
		if reports := stallwatch.Reports(dir); len(reports) > 0 {
			raw, _ := os.ReadFile(reports[0])
			report = string(raw)
		}
	}
	for _, want := range []string{StallSite, "store.TestAStatementThatDoesNotReturnIsInAStallReport.func"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	close(release)
	if err := <-listed; err != nil {
		t.Fatal(err)
	}
	for {
		raw, _ := os.ReadFile(stallwatch.Reports(dir)[0])
		if strings.Contains(string(raw), "ended:") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("report does not say the stall ended:\n%s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-stopped
	if got := len(stallwatch.Reports(dir)); got != 1 {
		t.Errorf("%d reports, want the one stall", got)
	}
}
