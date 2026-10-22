package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// NoticesPath is where the publisher's notices live in every source, beside
// the channels: one signed file for all channels, each notice saying itself
// which installations it is for.
const NoticesPath = "notices.json"

// NoticesSchema is the notices payload version this build reads.
const NoticesSchema = 1

// Notice severities, most urgent first.
const (
	SeverityCritical  = "critical"
	SeverityImportant = "important"
	SeverityInfo      = "info"
)

// Notice kinds.
const (
	KindSecurity = "security"
	KindRelease  = "release"
	KindEvent    = "event"
)

// Notices is the payload of notices.json: what the publisher wants the
// administrators of matching installations to read — security advisories,
// important releases, community events. A notice only informs; it never
// makes a server do anything.
type Notices struct {
	Schema      int       `json:"schema"`
	PublishedAt time.Time `json:"published_at"`
	Notices     []Notice  `json:"notices"`
}

// Notice is one message.
type Notice struct {
	// ID is stable for the life of the notice; read marks are kept by it.
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	// Title and Body are plain text per locale ("en" is required, the
	// fallback for every other locale).
	Title map[string]string `json:"title"`
	Body  map[string]string `json:"body,omitempty"`
	// URL is an https link to read more.
	URL       string    `json:"url,omitempty"`
	StartsAt  time.Time `json:"starts_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	// When narrows the installations the notice is for; empty means all.
	When NoticeWhen `json:"when,omitempty"`
}

// NoticeWhen are the conditions an installation must meet, all of them, for
// a notice to apply. The server decides from its own facts; nothing is sent
// to the publisher to decide.
type NoticeWhen struct {
	// Channels the server follows (nightly, release).
	Channels []string `json:"channels,omitempty"`
	// ServerBuiltBefore and ServerBuiltAfter bound the server's build time:
	// builds are ordered by when they were built, not by their version
	// strings (ADR 0040). A server whose build time is unknown (a local
	// build) matches neither bound.
	ServerBuiltBefore time.Time `json:"server_built_before,omitempty"`
	ServerBuiltAfter  time.Time `json:"server_built_after,omitempty"`
	// Vendors are accelerator vendors (internal/accelerator); the notice
	// applies when any node has cards of one of them.
	Vendors []string `json:"vendors,omitempty"`
}

// NoticeFacts is what an installation knows about itself to match notices.
type NoticeFacts struct {
	Channel string
	BuiltAt string // RFC 3339, version.BuildDate
	Vendors []string
	Now     time.Time
}

var noticeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Limits that keep a notice a notice, whatever the file says.
const (
	maxNotices     = 64
	maxNoticeTitle = 200
	maxNoticeBody  = 4000
)

// ParseNotices decodes and validates a verified notices payload. One bad
// notice refuses the whole file: the publisher must see the error, not have
// part of an advisory silently dropped.
func ParseNotices(payload []byte) (Notices, error) {
	var n Notices
	if err := json.Unmarshal(payload, &n); err != nil {
		return Notices{}, fmt.Errorf("decode notices: %w", err)
	}
	if err := n.Validate(); err != nil {
		return Notices{}, err
	}
	return n, nil
}

// Validate checks every notice.
func (n Notices) Validate() error {
	if n.Schema != NoticesSchema {
		return fmt.Errorf("notices schema %d is not supported (this build reads schema %d)", n.Schema, NoticesSchema)
	}
	if len(n.Notices) > maxNotices {
		return fmt.Errorf("%d notices, at most %d", len(n.Notices), maxNotices)
	}
	seen := map[string]bool{}
	for _, x := range n.Notices {
		if err := x.validate(); err != nil {
			return fmt.Errorf("notice %q: %w", x.ID, err)
		}
		if seen[x.ID] {
			return fmt.Errorf("notice %q appears twice", x.ID)
		}
		seen[x.ID] = true
	}
	return nil
}

func (x Notice) validate() error {
	if !noticeIDPattern.MatchString(x.ID) {
		return errors.New("id must be lowercase letters, digits, '.', '_' or '-'")
	}
	switch x.Severity {
	case SeverityCritical, SeverityImportant, SeverityInfo:
	default:
		return fmt.Errorf("severity %q is not critical, important or info", x.Severity)
	}
	switch x.Kind {
	case KindSecurity, KindRelease, KindEvent:
	default:
		return fmt.Errorf("kind %q is not security, release or event", x.Kind)
	}
	if strings.TrimSpace(x.Title["en"]) == "" {
		return errors.New("title needs an en text")
	}
	for locale, text := range x.Title {
		if len(text) > maxNoticeTitle {
			return fmt.Errorf("%s title is longer than %d bytes", locale, maxNoticeTitle)
		}
	}
	for locale, text := range x.Body {
		if len(text) > maxNoticeBody {
			return fmt.Errorf("%s body is longer than %d bytes", locale, maxNoticeBody)
		}
	}
	if x.URL != "" {
		u, err := url.Parse(x.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return fmt.Errorf("url %q is not an https link", x.URL)
		}
	}
	if x.ExpiresAt.IsZero() {
		return errors.New("expires_at is required: a notice must not stay forever")
	}
	if !x.StartsAt.IsZero() && !x.StartsAt.Before(x.ExpiresAt) {
		return errors.New("starts_at is not before expires_at")
	}
	for _, ch := range x.When.Channels {
		if !ValidChannel(ch) {
			return fmt.Errorf("channel %q is invalid", ch)
		}
	}
	return nil
}

// Applies reports whether the notice is for an installation with facts.
func (x Notice) Applies(f NoticeFacts) bool {
	if !x.StartsAt.IsZero() && f.Now.Before(x.StartsAt) {
		return false
	}
	if !f.Now.Before(x.ExpiresAt) {
		return false
	}
	w := x.When
	if len(w.Channels) > 0 && !containsFold(w.Channels, f.Channel) {
		return false
	}
	if !w.ServerBuiltBefore.IsZero() || !w.ServerBuiltAfter.IsZero() {
		built, err := time.Parse(time.RFC3339, strings.TrimSpace(f.BuiltAt))
		if err != nil {
			return false
		}
		if !w.ServerBuiltBefore.IsZero() && !built.Before(w.ServerBuiltBefore) {
			return false
		}
		if !w.ServerBuiltAfter.IsZero() && !built.After(w.ServerBuiltAfter) {
			return false
		}
	}
	if len(w.Vendors) > 0 {
		any := false
		for _, v := range f.Vendors {
			if containsFold(w.Vendors, v) {
				any = true
				break
			}
		}
		if !any {
			return false
		}
	}
	return true
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// FetchedNotices is a verified notices file and where it came from.
type FetchedNotices struct {
	Notices Notices
	Source  string
	KeyID   string
}

// FetchNotices asks every source at once for the notices file and returns the
// first that verifies. A source that has none answers 404; when every source
// that answered said so, the result is an empty file rather than an error.
func (c *Client) FetchNotices(ctx context.Context) (*FetchedNotices, error) {
	sources, err := c.sources()
	if err != nil {
		return nil, err
	}
	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		source  string
		notices Notices
		keyID   string
		err     error
	}
	results := make(chan result, len(sources))
	for _, source := range sources {
		go func(source string) {
			payload, keyID, err := c.signedFrom(raceCtx, source, NoticesPath, "")
			var n Notices
			if err == nil {
				n, err = ParseNotices(payload)
				if err != nil {
					err = fmt.Errorf("%s/%s: %w: %w", source, NoticesPath, ErrInvalidIndex, err)
				}
			}
			results <- result{source: source, notices: n, keyID: keyID, err: err}
		}(source)
	}
	var errs []error
	unpublished, invalid := false, false
	for range sources {
		var r result
		select {
		case r = <-results:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if r.err == nil {
			return &FetchedNotices{Notices: r.notices, Source: r.source, KeyID: r.keyID}, nil
		}
		var status *StatusError
		if errors.As(r.err, &status) && status.Status == http.StatusNotFound {
			unpublished = true
		}
		if errors.Is(r.err, ErrInvalidIndex) {
			invalid = true
		}
		errs = append(errs, r.err)
	}
	// Every source carries the same tree, so one "not found" means there are
	// no notices; a file that did not verify is never taken for none.
	if unpublished && !invalid {
		return &FetchedNotices{Notices: Notices{Schema: NoticesSchema}}, nil
	}
	return nil, fmt.Errorf("no distribution source served valid notices: %w", errors.Join(errs...))
}
