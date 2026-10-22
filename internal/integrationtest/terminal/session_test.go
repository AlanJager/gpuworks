package terminal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func terminalSessionTestToken(t *testing.T, st *store.Store, signer *auth.TokenSigner, role string) string {
	t.Helper()
	var user store.User
	err := st.DB().Where("username = ?", role).First(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		hash, hashErr := auth.HashPassword("password")
		if hashErr != nil {
			t.Fatal(hashErr)
		}
		user = store.User{Username: role, Role: role, PasswordHash: hash}
		if err := st.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	_, session, err := st.CreateManagementSession(&user, time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	token, err := signer.Sign(auth.Claims{UserID: user.ID, Username: user.Username, Role: user.Role, SessionID: session.TokenHash})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func terminalBrowserLogin(t *testing.T, server *httptest.Server) (*http.Cookie, string) {
	t.Helper()
	resp, err := http.Post(server.URL+"/api/v1/auth/login", "application/json", strings.NewReader(`{"username":"admin","password":"password"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status=%d", resp.StatusCode)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "gpuworks_session" {
			return cookie, body.Token
		}
	}
	t.Fatal("login did not set management cookie")
	return nil, ""
}

func TestTerminalSessionRevocationClosesConnection(t *testing.T) {
	for _, mode := range []string{"password-reset", "logout", "password-reset-other-store"} {
		t.Run(mode, func(t *testing.T) {
			h, server, nodeQuery := newHarness(t)
			node := strings.SplitN(nodeQuery, "?token=", 2)[0]
			cookie, token := terminalBrowserLogin(t, server)
			url := "ws" + strings.TrimPrefix(server.URL, "http") + "/api/v1/nodes/" + node + "/terminal?rows=40&cols=120"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}}})
			if err != nil {
				t.Fatal(err)
			}
			defer c.CloseNow()
			_, info, err := c.Read(ctx)
			if err != nil || !strings.Contains(string(info), `"type":"info"`) {
				t.Fatalf("terminal did not become ready: info=%s err=%v", info, err)
			}

			switch mode {
			case "password-reset":
				if err := h.store.SetUserPassword("admin", "new-password"); err != nil {
					t.Fatal(err)
				}
			case "logout":
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/auth/logout", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.AddCookie(cookie)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("logout status=%d", resp.StatusCode)
				}
			case "password-reset-other-store":
				// A separately opened Store has its own bus, matching an external
				// reset-password process rather than an in-process notification.
				other, err := store.Open("file:terminal-"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = other.Close() })
				if err := other.SetUserPassword("admin", "new-password"); err != nil {
					t.Fatal(err)
				}
			}

			closedCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			for {
				_, _, err := c.Read(closedCtx)
				if err != nil {
					if closedCtx.Err() != nil {
						t.Fatal("revoked terminal remained connected for 3 seconds")
					}
					break
				}
			}
			select {
			case <-h.agentDone:
			case <-closedCtx.Done():
				t.Fatal("revoked terminal leaked its agent stream")
			}
			for _, header := range []http.Header{
				{"Cookie": {cookie.Name + "=" + cookie.Value}},
				{"Authorization": {"Bearer " + token}},
			} {
				conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
				if conn != nil {
					_ = conn.CloseNow()
				}
				if resp != nil {
					_ = resp.Body.Close()
				}
				if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("revoked credential handshake response=%v err=%v, want 401", resp, err)
				}
			}
		})
	}
}
