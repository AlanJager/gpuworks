package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestImageLocalImportAndRetryKeepNodeFileSource(t *testing.T) {
	st, image := imageWithSources(t, "file:api_local_import?mode=memory&cache=shared", nil)
	signer, admin := roleTestToken(t, st, "admin")
	_, viewer := roleTestToken(t, st, "viewer")
	for _, retry := range []bool{false, true} {
		rec := &pullRecorder{}
		deps := rec.deps(st)
		deps.AuthSigner = signer
		var audit bytes.Buffer
		deps.Log = slog.New(slog.NewJSONHandler(&audit, nil))
		router := Router(deps)
		path := fmt.Sprintf("/api/v1/images/%d/nodes/node-1/sync", image.ID)
		if retry {
			path += "/retry"
		}
		for _, tc := range []struct {
			token string
			want  int
		}{{viewer, http.StatusForbidden}, {admin, http.StatusAccepted}} {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"source":" local ","archive_path":" /data/image with spaces.tar ","force":true}`))
			r.Header.Set("Authorization", "Bearer "+tc.token)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			if tc.want == http.StatusForbidden && (len(rec.pulls) != 0 || audit.Len() != 0) {
				t.Fatal("unauthorized request dispatched")
			}
		}
		if len(rec.pulls) != 1 || !rec.pulls[0].IsLocalFetch() || !rec.pulls[0].Force || rec.pulls[0].ArchivePath != "/data/image with spaces.tar" {
			t.Fatalf("unexpected fetch: %+v", rec.pulls)
		}
		var event map[string]any
		if err := json.Unmarshal(audit.Bytes(), &event); err != nil || event["username"] != "admin" || event["node_uuid"] != "node-1" || event["archive_path"] != rec.pulls[0].ArchivePath || event["action"] != "load" {
			t.Fatalf("audit = %s", audit.Bytes())
		}
	}
}

func TestImageLocalImportRejectsAmbiguousSources(t *testing.T) {
	st, image := imageWithSources(t, "file:api_local_invalid?mode=memory&cache=shared", nil)
	signer, admin := roleTestToken(t, st, "admin")
	for _, body := range []string{
		`{"source":"local"}`,
		`{"source":"local","archive_path":"relative.tar"}`,
		`{"archive_path":"/data/image.tar"}`,
		`{"source":"url","archive_path":"/data/image.tar"}`,
		`{"source":"local","archive_path":"/data/image.tar","source_node":"peer"}`,
		`{"source":"local","archive_path":"/data/image.tar","archive_url":"https://mirror/image.tar"}`,
		`{"source":"local","archive_path":"/data/image.tar","relay":true}`,
	} {
		for _, retry := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/retry=%v", body, retry), func(t *testing.T) {
				rec := &pullRecorder{}
				deps := rec.deps(st)
				deps.AuthSigner = signer
				path := fmt.Sprintf("/api/v1/images/%d/nodes/node-1/sync", image.ID)
				if retry {
					path += "/retry"
				}
				r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				r.Header.Set("Authorization", "Bearer "+admin)
				resp := httptest.NewRecorder()
				Router(deps).ServeHTTP(resp, r)
				var errorBody map[string]any
				if err := json.Unmarshal(resp.Body.Bytes(), &errorBody); err != nil || errorBody["code"] != "IMAGE_ARCHIVE_PATH_INVALID" {
					t.Fatalf("missing stable error code: %s", resp.Body.String())
				}
				if resp.Code != http.StatusBadRequest || len(rec.pulls)+len(rec.peers)+len(rec.retries) != 0 {
					t.Fatalf("invalid input dispatched: %d %s, %+v", resp.Code, resp.Body.String(), rec)
				}
			})
		}
	}
}
