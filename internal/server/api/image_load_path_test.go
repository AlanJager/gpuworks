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

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestImageLocalImportAndRetryKeepNodeFileSource(t *testing.T) {
	st, image := imageWithSources(t, "file:api_local_import?mode=memory&cache=shared", nil)
	signer, admin := roleTestToken(t, st, "admin")
	_, viewer := roleTestToken(t, st, "viewer")
	for _, action := range []string{"load", "replace", "retry"} {
		force := action == "replace"
		rec := &pullRecorder{}
		deps := rec.deps(st)
		deps.AuthSigner = signer
		var audit bytes.Buffer
		deps.Log = slog.New(slog.NewJSONHandler(&audit, nil))
		router := Router(deps)
		path := fmt.Sprintf("/api/v1/images/%d/nodes/node-1/sync", image.ID)
		if action == "retry" {
			path += "/retry"
		}
		for _, tc := range []struct {
			token string
			want  int
		}{{viewer, http.StatusForbidden}, {admin, http.StatusAccepted}} {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(fmt.Sprintf(`{"source":" local ","archive_path":" /data/image with spaces.tar ","force":%t}`, force)))
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
		if len(rec.pulls) != 1 || !rec.pulls[0].IsLocalFetch() || rec.pulls[0].Force != force || rec.pulls[0].ArchivePath != "/data/image with spaces.tar" {
			t.Fatalf("unexpected fetch: %+v", rec.pulls)
		}
		var event map[string]any
		if err := json.Unmarshal(audit.Bytes(), &event); err != nil || event["username"] != "admin" || event["node_uuid"] != "node-1" || event["archive_path"] != rec.pulls[0].ArchivePath || event["action"] != action || event["image_version_id"] != float64(image.ID) || event["force"] != force {
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

func TestViewerCanSyncFromRegistryDeclaredURLAndPeer(t *testing.T) {
	const archiveURL = "https://mirror.example/runner.tar"
	st, image := imageWithSources(t, "file:viewer_image_sync_sources?mode=memory&cache=shared", []string{archiveURL})
	signer, viewer := roleTestToken(t, st, "viewer")
	path := fmt.Sprintf("/api/v1/images/%d/nodes/node-1/sync", image.ID)
	for _, tc := range []struct{ name, body string }{
		{"registry", "{}"}, {"url", fmt.Sprintf(`{"source":"url","archive_url":%q}`, archiveURL)}, {"peer", `{"source_node":"node-peer"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &pullRecorder{}
			deps := rec.deps(st)
			deps.AuthSigner = signer
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+viewer)
			w := httptest.NewRecorder()
			Router(deps).ServeHTTP(w, r)
			if w.Code != http.StatusAccepted || len(rec.pulls)+len(rec.peers) != 1 {
				t.Fatalf("status=%d, dispatch=%+v", w.Code, rec)
			}
			switch tc.name {
			case "registry":
				if len(rec.pulls) != 1 || rec.pulls[0].Source != "" {
					t.Fatalf("registry dispatch=%+v", rec)
				}
			case "url":
				if len(rec.pulls) != 1 || !rec.pulls[0].IsURLFetch() || rec.pulls[0].ArchiveURL != archiveURL {
					t.Fatalf("URL dispatch=%+v", rec)
				}
			case "peer":
				if len(rec.peers) != 1 || rec.peers[0].from != "node-peer" || rec.peers[0].node != "node-1" {
					t.Fatalf("peer dispatch=%+v", rec)
				}
			}
		})
	}
}

func TestCatalogClassifiesStoredLocalImportFailures(t *testing.T) {
	a := apiHandlers{}
	nodes := []store.Node{{}}
	for message, want := range map[string]string{"legacy runtime detail": proto.ArchiveLoadFailed, proto.ArchivePreviewNotFound: proto.ArchivePreviewNotFound} {
		rows := []store.ImageNode{{State: store.ImageStateFailed, Source: proto.ArchiveSourceLocal, Message: message}}
		item := a.imageNodeDTOs(rows, nodes)[0]
		if item.TransferErrorCode != proto.ErrorArchiveLoad || item.Message != want {
			t.Fatalf("catalog failure = %+v", item)
		}
	}
}
