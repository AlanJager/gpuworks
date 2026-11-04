package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func imageInspectionTestSession(t *testing.T) (*store.Store, *auth.TokenSigner, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "management.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, _, _, err = st.EnsureAdmin("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	user, err := st.AuthenticateUser("admin", "gpuworks-test-password-2026")
	if err != nil {
		t.Fatal(err)
	}
	signer, err := auth.NewTokenSigner(filepath.Join(t.TempDir(), "auth-secret"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_, session, err := st.CreateManagementSession(user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, SessionID: session.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	return st, signer, token
}

func TestImageInspectionHTTPRedactsEnv(t *testing.T) {
	st, signer, token := imageInspectionTestSession(t)
	h := New(st, slog.Default())
	node, err := st.RegisterNode("inspect-node", store.HashToken("inspect-token"), "inspect-node")
	if err != nil {
		t.Fatal(err)
	}
	image, err := st.UpsertImage(&store.Image{ImageRef: "repo/image:tag", Engine: "vllm", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantEnv string
	}{
		{"legacy-agent", map[string]string{"API_TOKEN": "token-canary", "PASSWORD": "password-canary", "LOG_LEVEL": "info", "ENDPOINT": "https://user:pass@example.test/?token=url-canary"}, `,"env":{"API_TOKEN":"<redacted>","PASSWORD":"<redacted>","LOG_LEVEL":"info","ENDPOINT":"https://<redacted>@example.test/?token=<redacted>"}`},
		{"redacted-agent", map[string]string{"API_TOKEN": "<redacted>", "PASSWORD": "<redacted>", "LOG_LEVEL": "info"}, `,"env":{"API_TOKEN":"<redacted>","PASSWORD":"<redacted>","LOG_LEVEL":"info"}`},
		{"nil", nil, ""},
		{"empty", map[string]string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			session := &Session{NodeUUID: node.UUID, pending: make(map[uint64]chan proto.Envelope), closed: make(chan struct{})}
			session.sendFn = func(kind string, id uint64, data any) error {
				calls++
				req, ok := data.(rpcRequest)
				if !ok || kind != proto.TRPCReq || req.Method != proto.RPCImageInspect || req.Params != (proto.ImageInspectParams{ImageRef: image.ImageRef}) {
					t.Fatalf("unexpected RPC: %s %+v", kind, data)
				}
				raw, err := json.Marshal(map[string]any{"result": proto.ImageInspectResult{ImageRef: image.ImageRef, ImageID: "sha256:test", Entrypoint: []string{"vllm"}, Cmd: []string{"serve"}, Env: tc.env, Volumes: []string{"/model"}}})
				if err != nil {
					return err
				}
				session.dispatchRPCResp(proto.Envelope{ID: id, D: raw})
				return nil
			}
			h.sessions[node.UUID] = session
			router := api.Router(api.Deps{Store: st, AuthSigner: signer, InspectImage: h.InspectImage})
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/images/%d/nodes/%s/inspect", image.ID, node.UUID), nil)
			req.Header.Set("Authorization", "Bearer "+token)
			resp := httptest.NewRecorder()
			router.ServeHTTP(resp, req)
			if resp.Code != http.StatusOK || calls != 1 {
				t.Fatalf("status = %d, calls = %d, body = %s", resp.Code, calls, resp.Body.String())
			}
			wantJSON := `{"image_ref":"repo/image:tag","image_id":"sha256:test","entrypoint":["vllm"],"cmd":["serve"],"volumes":["/model"]` + tc.wantEnv + `}`
			var got, want map[string]any
			if err := json.Unmarshal(resp.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("wire response = %s, want %s", resp.Body.String(), wantJSON)
			}
			row, err := st.ImageNode(image.ID, node.ID)
			if err != nil || row.State != store.ImageStatePresent || row.ImageDigestID != "sha256:test" {
				t.Fatalf("image node status = %+v, error = %v", row, err)
			}
		})
	}
}
