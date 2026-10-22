package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"gpuworks.ai/gpuworks/internal/server/aihelper"
)

// maxAIReadBytes bounds one response the assistant reads in process; the
// tools cut what they show far below it.
const maxAIReadBytes = 8 << 20

var errAIReadTooLarge = errors.New("response larger than the assistant reads")

// aiReader is the assistant's aihelper.APIReader for one request: it serves
// GET requests through the same router, as the user who asked - the asker's
// Authorization header and cookies go with every read, so the auth
// middleware applies their permissions exactly as for the UI.
type aiReader struct {
	mux     *chi.Mux
	header  http.Header
	cookies []*http.Cookie
	routes  func() []string
	// log records every read with who asked; nil drops the record.
	log  *slog.Logger
	user string
}

// apiReaderFor is the reader for a request the asking user made.
func (a *apiHandlers) apiReaderFor(r *http.Request) aihelper.APIReader {
	header := http.Header{}
	if v := r.Header.Get("Authorization"); v != "" {
		header.Set("Authorization", v)
	}
	if v := r.Header.Get("Accept-Language"); v != "" {
		header.Set("Accept-Language", v)
	}
	user := ""
	if claims, ok := a.claimsFromRequest(r); ok {
		user = claims.Username
	}
	return &aiReader{mux: a.mux, header: header, cookies: r.Cookies(), routes: a.getRoutes, log: a.d.Log, user: user}
}

func (rd *aiReader) Route(path string) string {
	if rd.mux == nil {
		return ""
	}
	return rd.mux.Find(chi.NewRouteContext(), http.MethodGet, path)
}

func (rd *aiReader) Routes() []string { return rd.routes() }

func (rd *aiReader) Get(ctx context.Context, path string, query url.Values) (int, []byte, error) {
	if rd.mux == nil {
		return 0, nil, errors.New("api router not wired")
	}
	pattern := rd.Route(path)
	if pattern == "" || !aihelper.Readable(pattern) {
		return 0, nil, errors.New("not a readable API route: " + path)
	}
	target := path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	// The read runs inside the asker's own request, whose context carries
	// chi's routing state for /ai/ask; a mux that finds routing state routes
	// as a mounted sub-router and would resolve the read against the wrong
	// path. Each read starts its own routing.
	ctx = context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = rd.header.Clone()
	for _, c := range rd.cookies {
		req.AddCookie(c)
	}
	w := &capturedResponse{header: http.Header{}, status: http.StatusOK}
	start := time.Now().UTC()
	rd.mux.ServeHTTP(w, req)
	if rd.log != nil {
		rd.log.Info("ai api read", slog.String("user", rd.user), slog.String("route", pattern), slog.String("target", target),
			slog.Int("status", w.status), slog.Int("bytes", w.body.Len()), slog.Int64("ms", time.Since(start).Milliseconds()), slog.Bool("too_large", w.over))
	}
	if w.over {
		return w.status, nil, errAIReadTooLarge
	}
	return w.status, w.body.Bytes(), nil
}

// capturedResponse keeps one in-process response, up to maxAIReadBytes.
type capturedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	over   bool
}

func (c *capturedResponse) Header() http.Header  { return c.header }
func (c *capturedResponse) WriteHeader(code int) { c.status = code }
func (c *capturedResponse) Write(b []byte) (int, error) {
	if c.body.Len()+len(b) > maxAIReadBytes {
		c.over = true
		return 0, errAIReadTooLarge
	}
	return c.body.Write(b)
}

// getRoutes lists the GET route patterns under /api/v1, once.
func (a *apiHandlers) getRoutes() []string {
	a.routesOnce.Do(func() {
		if a.mux == nil {
			return
		}
		_ = chi.Walk(a.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
			if method == http.MethodGet && strings.HasPrefix(route, "/api/v1/") {
				a.routes = append(a.routes, route)
			}
			return nil
		})
		sort.Strings(a.routes)
	})
	return a.routes
}

// routesState is apiHandlers' cached route list.
type routesState struct {
	routesOnce sync.Once
	routes     []string
}
