//go:build linux && dqlite

package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// memberAskPrefix starts the member route's paths on which one member asks
// another's module a question (Host.AskMember). Like every request on the
// route it is served only to an admitted member's certificate; unlike a
// forwarded business request it is answered by the member it reaches,
// whether or not that member leads.
const memberAskPrefix = "/_member/ask/"

// serveAsk answers a peer's question to the module named in the path.
func (f *memberForward) serveAsk(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, memberAskPrefix)
	f.mu.Lock()
	answers := f.answers
	f.mu.Unlock()
	var answer MemberAnswerer
	if answers != nil && moduleName.MatchString(name) {
		answer = answers(name)
	}
	if r.Method != http.MethodPost || answer == nil {
		http.Error(w, "nothing on this member answers that", http.StatusNotFound)
		return
	}
	question, err := io.ReadAll(io.LimitReader(r.Body, MemberQuestionLimit+1))
	if err != nil || len(question) > MemberQuestionLimit {
		http.Error(w, "the question is too large", http.StatusRequestEntityTooLarge)
		return
	}
	out, err := answer(r.Context(), question)
	if err != nil || len(out) > MemberQuestionLimit {
		http.Error(w, "the member has no answer", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(out)
}

// ask sends question to module name on member over the member route and
// returns its answer. The connection is the route's own: this member's
// certificate towards the transport the cluster gives for that member.
func (f *memberForward) ask(ctx context.Context, member uint64, name string, question []byte) ([]byte, error) {
	if len(question) > MemberQuestionLimit {
		return nil, fmt.Errorf("a question to a member is at most %d bytes", MemberQuestionLimit)
	}
	f.mu.Lock()
	configured := f.peerTLS != nil
	f.mu.Unlock()
	if !configured {
		return nil, ErrMemberRouteNotReady
	}
	proxy := f.proxyTo(member)
	if proxy == nil {
		return nil, fmt.Errorf("%w: member %d has no route", ErrMemberUnreachable, member)
	}
	f.mu.Lock()
	address := f.routes[member]
	f.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+address+memberAskPrefix+name, bytes.NewReader(question))
	if err != nil {
		return nil, err
	}
	resp, err := proxy.Transport.RoundTrip(req)
	if err != nil {
		return nil, fmt.Errorf("%w: member %d: %v", ErrMemberUnreachable, member, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, MemberQuestionLimit+1))
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: member %d: %v", ErrMemberUnreachable, member, err)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%w: member %d answered %d", ErrMemberUnreachable, member, resp.StatusCode)
	case len(out) > MemberQuestionLimit:
		return nil, errors.New("the member's answer is too large")
	}
	return out, nil
}

// askMember is Host.AskMember on a cluster member.
func (a *App) askMember(ctx context.Context, member uint64, name string, question []byte) ([]byte, error) {
	runtime, ok := a.ha.(*memberRuntime)
	if !ok || runtime.bundle == nil {
		// Not on a replicated store, or never enrolled in a cluster.
		return nil, ErrNotClusterMember
	}
	if runtime.forward == nil {
		// The route is made when the business handler is, after the
		// modules' Init.
		return nil, ErrMemberRouteNotReady
	}
	return runtime.forward.ask(ctx, member, name, question)
}

// moduleName is the form of a module's name on the member route.
var moduleName = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)

// memberAnswerer is the function module name answers the other members
// with; nil when it has none.
func (a *App) memberAnswerer(name string) MemberAnswerer {
	if a == nil || a.ext == nil {
		return nil
	}
	return a.ext.answers[name]
}
