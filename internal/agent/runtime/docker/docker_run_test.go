package docker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

func TestRunFailureRemovesOnlyItsCreatedContainer(t *testing.T) {
	for _, tc := range []struct{ name, created, removeFailure string }{
		{name: "partially created", created: "new-id"},
		{name: "name conflict"},
		{name: "cleanup failure", created: "new-id", removeFailure: "exit 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			removed, record := filepath.Join(dir, "removed"), filepath.Join(dir, "record")
			installFakeDocker(t, `case "$1" in
run)
 while [ "$1" != --cidfile ]; do shift; done
 printf '%s' "$2" > '`+record+`'
 if [ -n '`+tc.created+`' ]; then printf '%s' '`+tc.created+`' > "$2"; fi
 echo 'launch failed' >&2; exit 125;;
rm) printf '%s' "$*" > '`+removed+`'; `+tc.removeFailure+`;;
esac`)
			id, err := New().Run(context.Background(), runtime.WorkloadSpec{Name: "existing-name", Image: "img"})
			if id != "" || err == nil || !strings.Contains(err.Error(), "launch failed") {
				t.Fatalf("id=%q err=%v", id, err)
			}
			data, _ := os.ReadFile(removed)
			if tc.created == "" && len(data) != 0 || tc.created != "" && string(data) != "rm -f new-id" {
				t.Fatalf("removal=%q", data)
			}
			if tc.removeFailure != "" && !strings.Contains(err.Error(), "remove failed docker container new-id") {
				t.Fatalf("cleanup failure missing: %v", err)
			}
			cidPath, readErr := os.ReadFile(record)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if _, err := os.Stat(filepath.Dir(string(cidPath))); !os.IsNotExist(err) {
				t.Fatalf("creation record directory leaked: %v", err)
			}
		})
	}
}

func TestTimedOutFailedRunWaitsForTheCreatedID(t *testing.T) {
	dir := t.TempDir()
	release, removed := filepath.Join(dir, "release"), filepath.Join(dir, "removed")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	installFakeDocker(t, `case "$1" in
run)
 while [ "$1" != --cidfile ]; do shift; done
 cid="$2"
 n=0; while [ ! -e '`+release+`' ] && [ "$n" -lt 150 ]; do n=$((n+1)); sleep 0.2; done
 printf late-created-id > "$cid"
 echo 'device start failed' >&2; exit 125;;
rm) printf '%s' "$*" > '`+removed+`';;
esac`)
	runTimeout, abandonGrace = 50*time.Millisecond, time.Minute
	t.Cleanup(func() { runTimeout, abandonGrace = 0, 0 })
	_, err := New().Run(context.Background(), runtime.WorkloadSpec{Name: "same-name", Image: "img"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err=%v", err)
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatalf("removed before CLI finished: %v", err)
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, _ := os.ReadFile(removed)
		if string(data) == "rm -f late-created-id" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("late failed run was not removed by ID: %q", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPendingRunCleanupRetriesOnlyItsCreatedID(t *testing.T) {
	dir := t.TempDir()
	retry, removed := filepath.Join(dir, "retry"), filepath.Join(dir, "removed")
	installFakeDocker(t, `case "$1" in
run)
 while [ "$1" != --cidfile ]; do shift; done
 printf retry-id > "$2"; echo 'device startup failed' >&2; exit 125;;
rm)
 printf '%s\n' "$*" >> '`+removed+`'
 if [ ! -e '`+retry+`' ]; then echo 'daemon unavailable' >&2; exit 1; fi;;
inspect) echo 'Error: No such object' >&2; exit 1;;
esac`)
	driver := New()
	spec := runtime.WorkloadSpec{Name: "needs-cleanup", Image: "img"}
	if _, err := driver.Run(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "remove failed") {
		t.Fatalf("Run error=%v", err)
	}
	if _, err := driver.Run(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("replacement bypassed cleanup: %v", err)
	}
	if _, err := driver.Status(t.Context(), spec.Name); err == nil || !strings.Contains(err.Error(), "cleanup pending") {
		t.Fatalf("Status accepted failed cleanup: %v", err)
	}
	if err := os.WriteFile(retry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	status, err := driver.Status(t.Context(), spec.Name)
	if err != nil || status.Exists {
		t.Fatalf("retry status=%+v, err=%v", status, err)
	}
	data, err := os.ReadFile(removed)
	if err != nil || strings.TrimSpace(string(data)) != "rm -f retry-id\nrm -f retry-id\nrm -f retry-id" {
		t.Fatalf("cleanup targets=%q, %v", data, err)
	}
}

func TestKilledRunWithoutIDKeepsCreationUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	release := filepath.Join(dir, "release")
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	installFakeDocker(t, `case "$1" in
run) n=0; while [ ! -e '`+release+`' ] && [ "$n" -lt 150 ]; do n=$((n+1)); sleep 0.2; done; echo too-late;;
*) echo 'Error: No such object' >&2; exit 1;;
esac`)
	runTimeout, abandonGrace = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { runTimeout, abandonGrace = 0, 0 })
	driver := New()
	spec := runtime.WorkloadSpec{Name: "unconfirmed-container", Image: "img"}
	if _, err := driver.Run(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Run error=%v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := driver.Status(t.Context(), spec.Name)
		if err != nil && strings.Contains(err.Error(), "creation remains unconfirmed") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed CLI was treated as confirmed absent: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := driver.Remove(t.Context(), spec.Name); err == nil || !strings.Contains(err.Error(), "creation remains unconfirmed") {
		t.Fatalf("Remove falsely confirmed absence: %v", err)
	}
	if _, err := driver.Run(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("replacement admitted after unconfirmed creation: %v", err)
	}
}

// A signalled CLI can leave the daemon's create request running just like a
// client killed after our own abandon grace. It cannot prove absence.
func TestExternallyKilledRunKeepsCreationUnconfirmed(t *testing.T) {
	installFakeDocker(t, `kill -KILL $$`)
	driver := New()
	spec := runtime.WorkloadSpec{Name: "externally-killed", Image: "img"}
	if _, err := driver.Run(t.Context(), spec); err == nil {
		t.Fatal("killed run succeeded")
	}
	if _, err := driver.Status(t.Context(), spec.Name); err == nil || !strings.Contains(err.Error(), "creation remains unconfirmed") {
		t.Fatalf("Status falsely confirmed absence: %v", err)
	}
	if err := driver.Remove(t.Context(), spec.Name); err == nil || !strings.Contains(err.Error(), "creation remains unconfirmed") {
		t.Fatalf("Remove falsely confirmed absence: %v", err)
	}
}
