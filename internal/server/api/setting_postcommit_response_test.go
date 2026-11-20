package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// Only notification I/O is held: the authenticated target transaction must
// finish normally, and GET must observe its exact wire value before release.
func TestSettingCommittedResponseDoesNotWaitForNotificationSQL(t *testing.T) {
	st, router, admin, _ := settingsFencingRouter(t)
	const key = "test.postcommit-response"
	const value = `[{"name":"one","enabled":true},{"name":"two","enabled":false},{"name":"three","enabled":true},{"name":"four","enabled":false}]`
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	var first sync.Once
	const callback = "test:setting-postcommit-response"
	if err := st.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if tx.Statement.Table != "committed_notifications" || transaction {
			return
		}
		first.Do(func() {
			select {
			case <-release:
				return // A scanner-only implementation needs no eager query.
			default:
			}
			close(entered)
			<-release
			// Simulate unavailable postcommit notification I/O after release. The
			// durable obligation must remain replayable; target success is unchanged.
			tx.AddError(errors.New("test: postcommit notification read unavailable"))
		})
	}); err != nil {
		t.Fatal(err)
	}
	defer st.DB().Callback().Query().Remove(callback)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- settingsFencingRequest(t, router, http.MethodPut, "/api/v1/settings/"+key, value, admin)
	}()
	var response *httptest.ResponseRecorder
	select {
	case <-entered:
	case response = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("actual postcommit notification query was not reached")
	}
	rec := settingsFencingRequest(t, router, http.MethodGet, "/api/v1/settings", "", admin)
	var values map[string]json.RawMessage
	if rec.Code != http.StatusOK {
		t.Fatalf("committed GET status=%d body=%s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &values); err != nil {
		t.Fatal(err)
	}
	if string(values[key]) != value {
		t.Fatalf("committed GET value=%s want=%s", values[key], value)
	}
	readNotice := func() store.CommittedNotification {
		t.Helper()
		var note store.CommittedNotification
		// Raw does not enter Query callbacks; this observes the durable row while
		// the publisher's distinct SELECT remains deliberately blocked.
		if err := st.DB().Raw("SELECT * FROM committed_notifications WHERE topic = ? AND resource_id = ?", "setting", key).Scan(&note).Error; err != nil {
			t.Fatal(err)
		}
		if note.RequestKey == "" || note.Published {
			t.Fatalf("notification obligation lost: %+v", note)
		}
		return note
	}
	pending := readNotice()
	t.Logf("target committed and exact GET visible; notification obligation pending revision=%d", pending.Revision)
	if response == nil {
		select {
		case response = <-done:
		case <-time.After(250 * time.Millisecond):
			t.Error("committed target is readable, but PUT saved response waits for blocked postcommit notification SQL")
		}
	}
	// Always release and join the handler, even on the expected RED baseline.
	unblock()
	if response == nil {
		select {
		case response = <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("PUT did not finish after notification SQL released")
		}
	}
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"status":"saved"}` {
		t.Fatalf("committed PUT status=%d body=%s", response.Code, response.Body)
	}
	t.Logf("actual PUT wire: status=%d body=%s", response.Code, strings.TrimSpace(response.Body.String()))
	after := readNotice()
	if after.RequestKey != pending.RequestKey || after.Revision != pending.Revision {
		t.Fatalf("pending changed after failed notification: before=%+v after=%+v", pending, after)
	}
	if n, err := st.ReconcileDroppedNotifications(t.Context()); err != nil || n != 1 {
		t.Fatalf("pending replay count=%d err=%v", n, err)
	}
	if n, err := st.ReconcileDroppedNotifications(t.Context()); err != nil || n != 0 {
		t.Fatalf("duplicate replay count=%d err=%v", n, err)
	}
	if got := st.GetSetting(key); got != value {
		t.Fatalf("notification replay changed committed target: %s", got)
	}
	t.Log("pending retained independently of eager notification I/O; scanner recovered once; repeated scan did not replay target")
}
