package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func TestFreshStoreHasOnlyTheAllNodesSyncGroupAndNoRules(t *testing.T) {
	s := newTestStore(t)
	rules, err := s.PullSourceRules()
	if err != nil || len(rules) != 0 {
		t.Fatalf("rules = %#v, err = %v; a fresh installation has none", rules, err)
	}
	groups, err := s.SyncGroups()
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Requirements) != 0 || groups[0].Name != DefaultSyncGroupName {
		t.Fatalf("groups = %#v, want only the seeded group selecting every node", groups)
	}
	if s.GetSetting(SettingImageRegistryRewrites) != "" {
		t.Fatal("a fresh installation must not be seeded with image registry rewrites")
	}
}

func TestPullSourceRuleCRUDOrderAndValidation(t *testing.T) {
	s := newTestStore(t)
	events := s.Bus.Subscribe(8, PullSettingsTopic)
	defer events.Close()

	first, err := s.CreatePullSourceRule(PullSourceRuleSpec{
		Match: " gpuworks/ ", Endpoints: []string{"registry.zstack.ai/gpuworks/", " "}, AllowUpstream: true, Note: " internal ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Match != "gpuworks/" || !reflect.DeepEqual(first.Endpoints, []string{"registry.zstack.ai/gpuworks/"}) || first.Note != "internal" || first.Position != 0 {
		t.Fatalf("created = %#v", first)
	}
	select {
	case ev := <-events.C():
		if ev.Key != PullSettingsRules {
			t.Fatalf("event key = %q", ev.Key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("creating a rule published nothing")
	}
	second, err := s.CreatePullSourceRule(PullSourceRuleSpec{
		Match: "docker.io", Endpoints: []string{"harbor.corp.example/dockerhub-proxy"}, AllowUpstream: false,
		Requirements: nodelabel.Requirements{"site": {"idc-a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Position != 1 {
		t.Fatalf("second position = %d, want after the first", second.Position)
	}
	rules, err := s.PullSourceRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].ID != first.ID || !reflect.DeepEqual(rules[1].Requirements, nodelabel.Requirements{"site": {"idc-a"}}) || rules[1].AllowUpstream {
		t.Fatalf("rules = %#v", rules)
	}

	if err := s.ReorderPullSourceRules([]uint{second.ID, first.ID}); err != nil {
		t.Fatal(err)
	}
	rules, _ = s.PullSourceRules()
	if rules[0].ID != second.ID || rules[1].ID != first.ID {
		t.Fatalf("reorder not reflected (stale cache?): %#v", rules)
	}
	var settingErr *PullSettingError
	if err := s.ReorderPullSourceRules([]uint{second.ID}); !errors.As(err, &settingErr) || settingErr.Field != "order" {
		t.Fatalf("partial order err = %v", err)
	}
	if err := s.ReorderPullSourceRules([]uint{second.ID, second.ID}); !errors.As(err, &settingErr) {
		t.Fatalf("duplicate order err = %v", err)
	}

	updated, err := s.UpdatePullSourceRule(first.ID, PullSourceRuleSpec{Match: "gpuworks/", Endpoints: []string{"a.example/gpuworks", "b.example/gpuworks"}, AllowUpstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Position != 1 || len(updated.Endpoints) != 2 {
		t.Fatalf("updated = %#v; an update keeps the position", updated)
	}
	rules, _ = s.PullSourceRules()
	if !reflect.DeepEqual(rules[1].Endpoints, []string{"a.example/gpuworks", "b.example/gpuworks"}) {
		t.Fatalf("endpoints after update = %#v", rules[1].Endpoints)
	}

	for name, bad := range map[string]PullSourceRuleSpec{
		"url match":         {Match: "https://docker.io", Endpoints: []string{"m.example"}, AllowUpstream: true},
		"tagged match":      {Match: "team/app:v1", Endpoints: []string{"m.example"}, AllowUpstream: true},
		"endpoint no host":  {Match: "docker.io", Endpoints: []string{"proxy/dockerhub"}, AllowUpstream: true},
		"duplicate":         {Match: "docker.io", Endpoints: []string{"m.example", "M.example/"}, AllowUpstream: true},
		"nothing to try":    {Match: "docker.io", AllowUpstream: true},
		"reserved label":    {Match: "docker.io", Endpoints: []string{"m.example"}, AllowUpstream: true, Requirements: nodelabel.Requirements{"gpuworks.ai/nope": {"x"}}},
		"empty requirement": {Match: "docker.io", Endpoints: []string{"m.example"}, AllowUpstream: true, Requirements: nodelabel.Requirements{"site": {}}},
	} {
		if _, err := s.CreatePullSourceRule(bad); !errors.As(err, &settingErr) {
			t.Errorf("%s: err = %v, want a PullSettingError", name, err)
		}
	}
	// A rule that only forbids a registry is valid: it offers nothing and
	// keeps nodes away from it.
	if _, err := s.CreatePullSourceRule(PullSourceRuleSpec{Match: "ghcr.io", AllowUpstream: false}); err != nil {
		t.Fatalf("block rule: %v", err)
	}

	if err := s.DeletePullSourceRule(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePullSourceRule(second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	if _, err := s.UpdatePullSourceRule(second.ID, PullSourceRuleSpec{Match: "docker.io", Endpoints: []string{"m.example"}, AllowUpstream: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a deleted rule err = %v", err)
	}
	var orphans int64
	s.db.Model(&PullSourceRuleEndpoint{}).Where("rule_id = ?", second.ID).Count(&orphans)
	var orphanReqs int64
	s.db.Model(&PullSourceRuleRequirement{}).Where("rule_id = ?", second.ID).Count(&orphanReqs)
	if orphans != 0 || orphanReqs != 0 {
		t.Fatalf("deleting a rule left %d endpoints and %d requirements", orphans, orphanReqs)
	}
}

func TestSyncGroupCRUD(t *testing.T) {
	s := newTestStore(t)
	groups, _ := s.SyncGroups()
	seededID := groups[0].ID

	idcA, err := s.CreateSyncGroup(SyncGroupSpec{Name: " 机房 A ", Requirements: nodelabel.Requirements{"site": {"idc-a"}}, RateLimitKiB: 204800})
	if err != nil {
		t.Fatal(err)
	}
	if idcA.Name != "机房 A" {
		t.Fatalf("created = %#v", idcA)
	}
	var settingErr *PullSettingError
	if _, err := s.CreateSyncGroup(SyncGroupSpec{Name: " "}); !errors.As(err, &settingErr) || settingErr.Field != "name" {
		t.Fatalf("unnamed group err = %v", err)
	}
	if _, err := s.CreateSyncGroup(SyncGroupSpec{Name: "x", RateLimitKiB: -1}); !errors.As(err, &settingErr) || settingErr.Field != "rate_limit" {
		t.Fatalf("negative rate err = %v", err)
	}
	// The seeded group is an ordinary one: it needs its name like any other,
	// and may be narrowed and deleted.
	if _, err := s.UpdateSyncGroup(seededID, SyncGroupSpec{Requirements: nodelabel.Requirements{"site": {"office"}}}); !errors.As(err, &settingErr) || settingErr.Field != "name" {
		t.Fatalf("seeded group without a name err = %v", err)
	}
	if _, err := s.UpdateSyncGroup(seededID, SyncGroupSpec{Name: "office", Requirements: nodelabel.Requirements{"site": {"office"}}}); err != nil {
		t.Fatal(err)
	}
	groups, _ = s.SyncGroups()
	if len(groups) != 2 || groups[0].ID != seededID || !reflect.DeepEqual(groups[0].Requirements, nodelabel.Requirements{"site": {"office"}}) || groups[1].RateLimitKiB != 204800 {
		t.Fatalf("groups = %#v", groups)
	}
	if err := s.DeleteSyncGroup(seededID); err != nil {
		t.Fatalf("the seeded group can be deleted: %v", err)
	}
	if err := s.DeleteSyncGroup(seededID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
	groups, _ = s.SyncGroups()
	if len(groups) != 1 || groups[0].ID != idcA.ID {
		t.Fatalf("groups after delete = %#v", groups)
	}
}

func TestSourceCredentialsEncryptAndKeepSecretOnEdit(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutSourceCredential(0, "harbor.corp.example", "ci", "pw"); err == nil {
		t.Fatal("a store without a secret key must refuse to store a secret")
	}
	box, err := auth.NewSecretbox("registry-credential-test")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSecretbox(box)

	created, err := s.PutSourceCredential(0, "https://Harbor.Corp.Example/", " ci-pull ", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if created.Host != "harbor.corp.example" || created.Username != "ci-pull" || !created.HasSecret {
		t.Fatalf("created = %#v", created)
	}
	var row SourceCredential
	s.db.First(&row, created.ID)
	if row.SecretCipher == "" || row.SecretCipher == "s3cret" {
		t.Fatalf("secret stored as %q, want ciphertext", row.SecretCipher)
	}
	hub, err := s.PutSourceCredential(0, "registry-1.docker.io", "me", "tok")
	if err != nil || hub.Host != "docker.io" {
		t.Fatalf("docker hub login = %#v, %v", hub, err)
	}

	// Renaming the user without a new secret keeps the old one.
	if _, err := s.PutSourceCredential(created.ID, "harbor.corp.example", "ci-pull-2", ""); err != nil {
		t.Fatal(err)
	}
	auths, err := s.SourceAuths()
	if err != nil {
		t.Fatal(err)
	}
	want := []proto.SourceAuth{
		{Host: "docker.io", Username: "me", Secret: "tok"},
		{Host: "harbor.corp.example", Username: "ci-pull-2", Secret: "s3cret"},
	}
	if !reflect.DeepEqual(auths, want) {
		t.Fatalf("auths = %#v, want %#v", auths, want)
	}

	var settingErr *PullSettingError
	for name, call := range map[string]func() error{
		"duplicate host": func() error { _, err := s.PutSourceCredential(0, "harbor.corp.example", "x", "y"); return err },
		"no secret":      func() error { _, err := s.PutSourceCredential(0, "other.example", "x", ""); return err },
		"path":           func() error { _, err := s.PutSourceCredential(0, "other.example/team", "x", "y"); return err },
		"not a host":     func() error { _, err := s.PutSourceCredential(0, "harbor", "x", "y"); return err },
	} {
		if err := call(); !errors.As(err, &settingErr) {
			t.Errorf("%s: err = %v, want a PullSettingError", name, err)
		}
	}
	// A model site's token has no user name.
	site, err := s.PutSourceCredential(0, "HuggingFace.co", " ", "hf_token")
	if err != nil || site.Host != "huggingface.co" || site.Username != "" || !site.HasSecret {
		t.Fatalf("site token = %#v, %v", site, err)
	}
	if err := s.DeleteSourceCredential(site.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutSourceCredential(9999, "other.example", "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("edit of a missing login err = %v", err)
	}

	// A login whose secret no longer decrypts (the server key changed) is
	// left out of what nodes are sent, and testing it says why.
	other, err := auth.NewSecretbox("another-key")
	if err != nil {
		t.Fatal(err)
	}
	s.SetSecretbox(other)
	if auths, err := s.SourceAuths(); err != nil || len(auths) != 0 {
		t.Fatalf("auths under a different key = %#v, %v", auths, err)
	}
	if _, err := s.SourceAuthFor(created.ID); err == nil {
		t.Fatal("SourceAuthFor decrypted under the wrong key")
	}

	if err := s.DeleteSourceCredential(created.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSourceCredential(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestPullSettingsCacheIsSharedAcrossReads(t *testing.T) {
	s, err := Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreatePullSourceRule(PullSourceRuleSpec{Match: "docker.io", Endpoints: []string{"m.example"}, AllowUpstream: true}); err != nil {
		t.Fatal(err)
	}
	a, _ := s.PullSourceRules()
	// A row written behind the store's back is not seen until a store write
	// invalidates the cache: reads are served from memory.
	if err := s.db.Model(&PullSourceRule{}).Where("id = ?", a[0].ID).Update("match", "quay.io").Error; err != nil {
		t.Fatal(err)
	}
	b, _ := s.PullSourceRules()
	if b[0].Match != "docker.io" {
		t.Fatalf("read went to the database: %#v", b)
	}
	if _, err := s.CreateSyncGroup(SyncGroupSpec{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	c, _ := s.PullSourceRules()
	if c[0].Match != "quay.io" {
		t.Fatalf("a write did not invalidate the cache: %#v", c)
	}
}
