package distribution

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func editionIndex(channel, edition string) Index {
	index := testIndex(channel)
	index.Edition = edition
	return index
}

func indexPayload(t *testing.T, index Index) []byte {
	t.Helper()
	payload, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestParseIndexForMatchesEditions(t *testing.T) {
	cases := []struct {
		name, published, want string
		ok                    bool
	}{
		{"absent edition is community", "", "", true},
		{"absent edition read by community", "", "community", true},
		{"named community read with empty", "community", "", true},
		{"acme read by acme", "acme", "acme", true},
		{"community refused by acme", "", "acme", false},
		{"acme refused by community", "acme", "", false},
		{"acme refused by another edition", "acme", "oem-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIndexFor(indexPayload(t, editionIndex("nightly", tc.published)), "nightly", tc.want)
			if tc.ok {
				if err != nil || got.Edition != tc.published {
					t.Fatalf("ParseIndexFor = %+v, %v", got, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "edition") {
				t.Fatalf("err = %v, want an edition mismatch", err)
			}
		})
	}
}

func TestParseIndexIsCommunity(t *testing.T) {
	if _, err := ParseIndex(indexPayload(t, testIndex("nightly")), "nightly"); err != nil {
		t.Fatalf("a community index: %v", err)
	}
	_, err := ParseIndex(indexPayload(t, editionIndex("nightly", "acme")), "nightly")
	if err == nil || !strings.Contains(err.Error(), "for the acme edition, not community") {
		t.Fatalf("err = %v", err)
	}
}

func TestIndexValidateRefusesBadEdition(t *testing.T) {
	for _, bad := range []string{"Acme", "acme edition", "-x", strings.Repeat("a", 33)} {
		if err := editionIndex("nightly", bad).Validate(); err == nil || !strings.Contains(err.Error(), "edition") {
			t.Errorf("edition %q: err = %v", bad, err)
		}
	}
}

// The edition is on the wire only when it is set, so a community index is
// byte-for-byte what clients from before editions read.
func TestIndexEditionWire(t *testing.T) {
	community, _ := json.Marshal(testIndex("nightly"))
	if strings.Contains(string(community), `"edition"`) {
		t.Fatalf("community index carries an edition: %s", community)
	}
	acme, _ := json.Marshal(editionIndex("nightly", "acme"))
	if !strings.Contains(string(acme), `"channel":"nightly","edition":"acme","published_at"`) {
		t.Fatalf("edition index wire = %s", acme)
	}
}

func TestCommunityProfile(t *testing.T) {
	p := Community()
	if p.Edition != EditionCommunity || !reflect.DeepEqual(p.Sources, DefaultSources) || len(p.Keys) != len(TrustedKeys()) {
		t.Fatalf("Community() = %+v", p)
	}
	p.Sources[0] = "https://changed.example"
	if DefaultSources[0] == "https://changed.example" {
		t.Fatal("Community() shares DefaultSources")
	}
}

func TestProfileClient(t *testing.T) {
	httpClient := &http.Client{}
	zero := Profile{}.Client(httpClient)
	if zero.Edition != EditionCommunity || !reflect.DeepEqual(zero.Sources, DefaultSources) || !zero.Published || zero.HTTP != httpClient || zero.Keys == nil {
		t.Fatalf("zero profile client = %+v", zero)
	}
	acme := Profile{Edition: "acme", Sources: []string{"https://acme.example"}, Keys: testKeys()}.Client(nil)
	if acme.Edition != "acme" || !reflect.DeepEqual(acme.Sources, []string{"https://acme.example"}) || !acme.Published || acme.HTTP != nil {
		t.Fatalf("acme profile client = %+v", acme)
	}
	// An edition without sources of its own reads the built-in sources, where
	// its edition is never published: every index there is refused.
	bare := Profile{Edition: "acme"}.Client(nil)
	if !reflect.DeepEqual(bare.Sources, DefaultSources) || bare.Edition != "acme" {
		t.Fatalf("bare profile client = %+v", bare)
	}
}

// A community index on an edition's profile is refused even though it is
// signed by a key the profile trusts.
func TestProfileClientRefusesAnotherEditionsIndex(t *testing.T) {
	store := newObjectStore(t)
	publish(t, testIndex("release"), store)
	client := Profile{Edition: "acme", Sources: []string{store.URL()}, Keys: testKeys()}.Client(nil)
	client.IndexTimeout = 2 * time.Second
	_, err := client.FetchIndex(context.Background(), "release")
	var sourcesErr *SourcesError
	if !errors.As(err, &sourcesErr) || sourcesErr.Reason() != ReasonInvalid || !strings.Contains(err.Error(), "for the community edition, not acme") {
		t.Fatalf("err = %v", err)
	}

	publish(t, editionIndex("release", "acme"), store)
	fetched, err := client.FetchIndex(context.Background(), "release")
	if err != nil || fetched.Index.Edition != "acme" {
		t.Fatalf("fetch = %+v, %v", fetched, err)
	}
	// And the community client refuses the acme index now there.
	_, err = clientFor(store).FetchIndex(context.Background(), "release")
	if err == nil || !strings.Contains(err.Error(), "for the acme edition, not community") {
		t.Fatalf("community client err = %v", err)
	}
}

// Notices follow the profile: an edition's client asks only its own sources,
// so it never shows the community bell.
func TestProfileClientNeverFetchesCommunityNotices(t *testing.T) {
	community, acme := newObjectStore(t), newObjectStore(t)
	publishNotices(t, Notices{Schema: NoticesSchema, PublishedAt: noticeNow, Notices: []Notice{testNotice("community-only")}}, community)
	publishNotices(t, Notices{Schema: NoticesSchema, PublishedAt: noticeNow, Notices: []Notice{testNotice("acme-only")}}, acme)
	useDefaultSources(t, community)
	client := Profile{Edition: "acme", Sources: []string{acme.URL()}, Keys: testKeys()}.Client(nil)
	got, err := client.FetchNotices(context.Background())
	if err != nil || len(got.Notices.Notices) != 1 || got.Notices.Notices[0].ID != "acme-only" {
		t.Fatalf("notices = %+v, %v", got, err)
	}
	if reqs := community.requestLog(); len(reqs) != 0 {
		t.Fatalf("the community source was asked: %q", reqs)
	}
}

// Update checks report to an edition's own published sources as they do to
// the built-in ones, and to nobody once an operator's mirror replaces them.
func TestProfileClientReportsToPublishedSources(t *testing.T) {
	store := newObjectStore(t)
	publish(t, editionIndex("nightly", "acme"), store)
	client := Profile{Edition: "acme", Sources: []string{store.URL()}, Keys: testKeys()}.Client(nil)
	if _, err := client.FetchIndexReporting(context.Background(), "nightly", sampleReport()); err != nil {
		t.Fatal(err)
	}
	if q := store.queryLog(); len(q) != 1 || !strings.Contains(q[0], "id=0123456789abcdef") {
		t.Fatalf("published source queries = %q", q)
	}
	client.Published = false
	if _, err := client.FetchIndexReporting(context.Background(), "nightly", sampleReport()); err != nil {
		t.Fatal(err)
	}
	if q := store.queryLog(); len(q) != 2 || q[1] != "" {
		t.Fatalf("mirror queries = %q", q)
	}
}

func TestEditionNames(t *testing.T) {
	if NormalizeEdition(" ") != EditionCommunity || NormalizeEdition("acme") != "acme" {
		t.Fatal("NormalizeEdition")
	}
	if !SameEdition("", "community") || SameEdition("", "acme") {
		t.Fatal("SameEdition")
	}
	if !ValidEdition("") || !ValidEdition("oem-a") || ValidEdition("OEM") {
		t.Fatal("ValidEdition")
	}
}
