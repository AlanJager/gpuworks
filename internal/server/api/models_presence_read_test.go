package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelPresenceReadWire(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	node, err := rig.st.RegisterNode("presence-node", store.HashToken("presence-node"), "presence-node")
	if err != nil {
		t.Fatal(err)
	}
	model, err := rig.st.UpsertModel(&store.Model{Source: "local", SourceID: "/m/presence", Name: "presence", README: "must not leave this read"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rig.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFDownloading, SizeBytes: 100, Downloaded: 40})
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/api/v1/models/presence?ids=%d,%d,999999", model.ID, model.ID)
	rec := rig.do(rig.session, http.MethodGet, path, "")
	want := fmt.Sprintf(`{"%d":{"ready_nodes":0,"cached_nodes":0,"ready_storages":0,"in_flight":1,"in_flight_percent":40,"failed":0}}`, model.ID)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("presence=%d %s; want %s", rec.Code, rec.Body, want)
	}
	admin, err := rig.st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	user := store.User{Username: "presence-reader", PasswordHash: admin.PasswordHash, Role: "user"}
	if err := rig.st.DB().Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	login := rig.do("", http.MethodPost, "/api/v1/auth/login", `{"username":"presence-reader","password":"gpuworks-test-password-2026"}`)
	var session struct {
		Token string `json:"token"`
	}
	if login.Code != 200 || json.Unmarshal(login.Body.Bytes(), &session) != nil {
		t.Fatalf("login=%d %s", login.Code, login.Body)
	}
	if got := rig.do(session.Token, http.MethodGet, path, ""); got.Code != 200 || strings.TrimSpace(got.Body.String()) != want {
		t.Fatalf("ordinary management reader=%d %s", got.Code, got.Body)
	}
	for _, tc := range []struct {
		method, path, token string
		status              int
	}{
		{http.MethodGet, path, "", http.StatusUnauthorized},
		{http.MethodPost, path, rig.session, http.StatusMethodNotAllowed},
		{http.MethodGet, "/api/v1/models/presence", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=0", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=x", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=18446744073709551615", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=1,", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=" + strings.Repeat("1,", 200) + "1", rig.session, http.StatusBadRequest},
		{http.MethodGet, "/api/v1/models/presence?ids=999999", rig.session, http.StatusOK},
	} {
		if got := rig.do(tc.token, tc.method, tc.path, ""); got.Code != tc.status {
			t.Errorf("%s %s = %d %s; want %d", tc.method, tc.path, got.Code, got.Body, tc.status)
		}
	}
	for _, table := range []string{"models", "storages", "nodes"} {
		const hook = "test:presence-read-failure"
		if err := rig.st.DB().Callback().Query().Before("gorm:query").Register(hook, func(db *gorm.DB) {
			if db.Statement.Table == table {
				db.AddError(errors.New("injected presence read failure"))
			}
		}); err != nil {
			t.Fatal(err)
		}
		got := rig.do(rig.session, http.MethodGet, path, "")
		if err := rig.st.DB().Callback().Query().Remove(hook); err != nil {
			t.Fatal(err)
		}
		if got.Code != http.StatusInternalServerError {
			t.Errorf("%s read failure=%d %s", table, got.Code, got.Body)
		}
	}
	if err := rig.st.DB().Migrator().DropTable(&store.ModelFile{}); err != nil {
		t.Fatal(err)
	}
	if got := rig.do(rig.session, http.MethodGet, path, ""); got.Code != http.StatusInternalServerError {
		t.Fatalf("database failure=%d %s", got.Code, got.Body)
	}
}

func TestModelPresenceReadIsTargetedAndUsesConstantBatchQueries(t *testing.T) {
	f := newModelListFixture(t)
	models, err := f.st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int64
	var statements []string
	const hook = "test:model-presence-batch"
	if err := f.st.DB().Callback().Query().After("gorm:query").Register(hook, func(db *gorm.DB) {
		queries.Add(1)
		statements = append(statements, db.Statement.SQL.String())
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.st.DB().Callback().Query().Remove(hook) })
	read := func(ids []uint) (int64, map[uint]dto.ModelPresenceSummary) {
		t.Helper()
		parts := make([]string, len(ids))
		for i, id := range ids {
			parts[i] = fmt.Sprint(id)
		}
		queries.Store(0)
		statements = nil
		rec := httptest.NewRecorder()
		f.handler.modelPresence(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models/presence?ids="+strings.Join(parts, ","), nil))
		var result map[uint]dto.ModelPresenceSummary
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			t.Fatalf("read=%d %s", rec.Code, rec.Body)
		}
		for _, sql := range statements {
			if strings.Contains(sql, "FROM `models`") && !strings.Contains(sql, "SELECT `id`") {
				t.Errorf("model metadata read: %s", sql)
			}
			if strings.Contains(sql, "FROM `model_files`") && !strings.Contains(sql, "model_id IN") {
				t.Errorf("unbounded file read: %s", sql)
			}
		}
		return queries.Load(), result
	}
	var occupied []uint
	for _, m := range models {
		if m.Source != "local" {
			occupied = append(occupied, m.ID)
		}
	}
	few, single := read(occupied[:1])
	if len(single) != 1 {
		t.Fatalf("off-page model leaked: %v", single)
	}
	many, all := read(occupied)
	if few != 4 || many != few {
		t.Fatalf("queries for one/two models = %d/%d; want fixed four", few, many)
	}
	for _, m := range models {
		if m.Source != "local" {
			want := modelPresenceSummary(f.handler.modelDTO(&m, true))
			if !reflect.DeepEqual(all[m.ID], want) {
				t.Errorf("model %d summary=%+v; want %+v", m.ID, all[m.ID], want)
			}
		} else {
			_, empty := read([]uint{m.ID})
			if got, present := empty[m.ID]; !present || got != (dto.ModelPresenceSummary{}) {
				t.Errorf("known empty model=%v", empty)
			}
		}
	}
}
