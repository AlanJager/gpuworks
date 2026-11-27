package api

import (
	"net/http"

	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// managementAdmin guards the standalone enrollment listener with the same
// live session check the main API uses. A signed token alone is not enough.
func managementAdmin(st *store.Store, signer *auth.TokenSigner) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := claimsFromRequest(st, signer, r)
			if !ok {
				writeProductError(w, http.StatusUnauthorized, ErrorAuthenticationRequired, "authentication required", nil)
				return
			}
			if claims.Role != "admin" {
				writeProductError(w, http.StatusForbidden, ErrorAdministratorRequired, "administrator role required", nil)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func claimsFromRequest(st *store.Store, signer *auth.TokenSigner, r *http.Request) (auth.Claims, bool) {
	if st == nil || signer == nil {
		return auth.Claims{}, false
	}
	hash, err := enrollmentSessionHash(signer, r)
	if err != nil {
		return auth.Claims{}, false
	}
	user, _, err := st.ManagementSessionByHash(hash)
	if err != nil || user == nil {
		return auth.Claims{}, false
	}
	if bearer := auth.BearerToken(r.Header.Get("Authorization")); bearer != "" {
		claims, err := signer.Verify(bearer)
		if err != nil || claims.UserID != user.ID {
			return auth.Claims{}, false
		}
	}
	return auth.Claims{
		UserID: user.ID, Username: user.Username, Role: user.Role,
		SessionID: hash, LocalAdmin: user.IsLocalAdmin(),
	}, true
}

func enrollmentSessionHash(signer *auth.TokenSigner, r *http.Request) (string, error) {
	if bearer := auth.BearerToken(r.Header.Get("Authorization")); bearer != "" {
		claims, err := signer.Verify(bearer)
		if err != nil {
			return "", err
		}
		if claims.SessionID == "" {
			return "", auth.ErrInvalidToken
		}
		return claims.SessionID, nil
	}
	cookie, err := r.Cookie(managementCookie)
	if err != nil || cookie.Value == "" || len(cookie.Value) != 64 {
		return "", auth.ErrInvalidToken
	}
	return store.HashToken(cookie.Value), nil
}
