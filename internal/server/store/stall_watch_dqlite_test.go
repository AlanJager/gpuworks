//go:build linux && dqlite

package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// On the replicated store the writer's pool begins a transaction through the
// cluster's writer admission, not through database/sql. The watched pool
// must keep that path, and a write that waits for the admission another
// write holds, which is where a member's writes queue while the database
// process waits for the disk or for a majority, must be in a stall report.
func TestNativeWriteWaitingForTheWriterAdmissionIsInAStallReport(t *testing.T) {
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := OpenRuntimeWith(ctx, filepath.Join(dir, "gpuworks.db"), bus.New(), OpenOptions{Backend: ReplicatedBackend()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	watched, ok := st.db.Statement.ConnPool.(*watchedPool)
	if !ok {
		t.Fatalf("the replicated store's pool is %T, which the stall watch does not see", st.db.Statement.ConnPool)
	}
	if _, ok := watched.ConnPool.(*replicatedWriterPool); !ok {
		t.Fatalf("the watched pool wraps %T, not the replicated writer's pool", watched.ConnPool)
	}
	// As before the pool was watched, the replicated writer's pool is not
	// database/sql's: a lookup with its cards runs its statements on it
	// directly instead of holding one connection (withCards).
	if isDatabasePool(st.db.Statement.ConnPool) {
		t.Fatal("the replicated writer's pool is now taken for database/sql's pool")
	}
	if _, err := st.RegisterNode("uuid-native", HashToken("token-native"), "host-native"); err != nil {
		t.Fatal(err)
	}
	stalls := startStallWatch(t)

	// One write holds the admission; the second waits for it in BeginTx.
	holding, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- st.db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Exec("UPDATE nodes SET hostname = 'host-first' WHERE uuid = 'uuid-native'").Error; err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	second := make(chan error, 1)
	go func() {
		second <- st.db.Transaction(func(tx *gorm.DB) error {
			return tx.Exec("UPDATE nodes SET hostname = 'host-second' WHERE uuid = 'uuid-native'").Error
		})
	}()
	report := stallReport(t, stalls, "goroutines:")
	for _, want := range []string{"  " + StallSiteBegin + "\n", "store.(*watchedPool).BeginTx", "store.(*replicatedWriterPool).BeginTx", "cluster.BeginWriterTransaction"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the write that held the admission: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("the write that waited for the admission: %v", err)
	}
	stallReport(t, stalls, "ended:")
	node, err := st.NodeByUUID("uuid-native")
	if err != nil || node.Hostname != "host-second" {
		t.Fatalf("node after both writes: %+v, %v", node, err)
	}
}

// On the replicated store too, every call into the watched pool ends the
// wait it began: the writer's pool with its own way of beginning a
// transaction, and the pool the scheduler reads on.
func TestNativeEveryWatchedCallEndsItsWait(t *testing.T) {
	ctx := dqliteTestContext(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := OpenRuntimeWith(ctx, filepath.Join(dir, "gpuworks.db"), bus.New(), OpenOptions{Backend: ReplicatedBackend()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("uuid-native-ends", HashToken("token-native-ends"), "host-native-ends")
	if err != nil {
		t.Fatal(err)
	}
	w := idleWatch(t)
	exerciseWatchedPool(t, w, st.db)
	if _, err := st.NodeByID(node.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ListNodes(); err != nil {
		t.Fatal(err)
	}
	leftOpen(t, w, "lookups with their cards on the replicated store")
	if reader := st.reader(); reader != st.db {
		if _, watched := reader.Statement.ConnPool.(*watchedPool); !watched {
			t.Fatalf("the replicated read pool is %T, which the stall watch does not see", reader.Statement.ConnPool)
		}
		var count int64
		if err := reader.Model(&Node{}).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		leftOpen(t, w, "a query on the replicated read pool")
	}
}
