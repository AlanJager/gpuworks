package identity

import (
	"encoding/json"
	"errors"

	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gpuworks.ai/gpuworks/internal/filelock"
)

func policyFixture(t *testing.T) (string, TransportPolicy) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "transport.json"), TransportPolicy{ClusterID: strings.Repeat("1", 32), CAFingerprint: strings.Repeat("2", 64), Revision: 1, AllowedKeys: []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}}
}

func TestTransportPolicyPublicationRestartAndRevocation(t *testing.T) {
	path, p := policyFixture(t)
	if err := PublishTransportPolicy(path, 0, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTransportPolicy(path)
	if err != nil || !reflect.DeepEqual(p, loaded) {
		t.Fatalf("load: %#v %v", loaded, err)
	}
	authorize, err := loaded.Authorizer(p.ClusterID, p.CAFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := p.AllowedKeys[0]
	loaded.AllowedKeys[0] = strings.Repeat("c", 64)
	if err := authorize(oldKey); err != nil {
		t.Fatal("callback mutated", err)
	}
	if err := authorize(loaded.AllowedKeys[0]); err == nil {
		t.Fatal("mutated key authorized")
	}
	next := p
	next.Revision = 2
	next.AllowedKeys = []string{p.AllowedKeys[1]}
	next.RevokedKeys = []string{oldKey}
	if err := PublishTransportPolicy(path, 1, next); err != nil {
		t.Fatal(err)
	}
	if err := PublishTransportPolicy(path, 1, next); err != nil {
		t.Fatal("uncertain commit confirmation", err)
	}
	if err := PublishTransportPolicy(path, 0, p); err == nil {
		t.Fatal("old update accepted")
	}
	restarted, err := LoadTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := restarted.Authorizer(p.ClusterID, p.CAFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if after(oldKey) == nil || after(next.AllowedKeys[0]) != nil {
		t.Fatal("restart authorization incorrect")
	}
	if authorize(oldKey) != nil {
		t.Fatal("running snapshot was changed")
	}
	if _, err := restarted.Authorizer(strings.Repeat("3", 32), p.CAFingerprint); err == nil {
		t.Fatal("wrong cluster accepted")
	}
	if _, err := restarted.Authorizer(p.ClusterID, strings.Repeat("3", 64)); err == nil {
		t.Fatal("wrong authority accepted")
	}
	third := next
	third.Revision = 3
	third.RevokedKeys = nil
	if err := PublishTransportPolicy(path, 2, third); err == nil {
		t.Fatal("tombstone discarded")
	}
	third.AllowedKeys = p.AllowedKeys
	if err := PublishTransportPolicy(path, 2, third); err == nil {
		t.Fatal("revoked member readmitted")
	}
	got, _ := LoadTransportPolicy(path)
	if !reflect.DeepEqual(got, next) {
		t.Fatal("failed update changed state")
	}
}

func TestTransportPolicyRejectsInvalidUpdates(t *testing.T) {
	cases := map[string]func(*TransportPolicy){
		"empty":             func(p *TransportPolicy) { p.AllowedKeys = nil },
		"unsorted":          func(p *TransportPolicy) { p.AllowedKeys = []string{strings.Repeat("b", 64), strings.Repeat("a", 64)} },
		"duplicate":         func(p *TransportPolicy) { p.AllowedKeys = []string{strings.Repeat("a", 64), strings.Repeat("a", 64)} },
		"uppercase":         func(p *TransportPolicy) { p.AllowedKeys = []string{strings.Repeat("A", 64)} },
		"short":             func(p *TransportPolicy) { p.AllowedKeys = []string{"aa"} },
		"overlap":           func(p *TransportPolicy) { p.RevokedKeys = []string{p.AllowedKeys[0]} },
		"authority":         func(p *TransportPolicy) { p.CAFingerprint = strings.Repeat("3", 64) },
		"cluster":           func(p *TransportPolicy) { p.ClusterID = strings.Repeat("3", 32) },
		"missing tombstone": func(p *TransportPolicy) { p.AllowedKeys = p.AllowedKeys[1:] },
		"skipped revision":  func(p *TransportPolicy) { p.Revision = 3 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			path, p := policyFixture(t)
			if err := PublishTransportPolicy(path, 0, p); err != nil {
				t.Fatal(err)
			}
			next := p
			next.Revision = 2
			change(&next)
			if err := PublishTransportPolicy(path, 1, next); err == nil {
				t.Fatal("invalid update accepted")
			}
			got, err := LoadTransportPolicy(path)
			if err != nil || !reflect.DeepEqual(got, p) {
				t.Fatal("invalid update changed original", err)
			}
		})
	}
}

func TestTransportPolicyMalformedFileNeverRepaired(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"case-folded-field": func(b []byte) []byte { return []byte(strings.Replace(string(b), `"revision"`, `"REVISION"`, 1)) },
		"missing-revocations": func(b []byte) []byte {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(b, &fields)
			delete(fields, "revoked_keys")
			result, _ := json.Marshal(fields)
			return result
		},
		"empty":     func([]byte) []byte { return nil },
		"partial":   func(b []byte) []byte { return b[:len(b)/2] },
		"unknown":   func(b []byte) []byte { return append([]byte(`{"unexpected":true,`), b[1:]...) },
		"duplicate": func(b []byte) []byte { return append([]byte(`{"revision":1,`), b[1:]...) },
		"trailing":  func(b []byte) []byte { return append(b, []byte(` {}`)...) },
		"null":      func([]byte) []byte { return []byte(`null`) },
		"oversized": func([]byte) []byte { return []byte(strings.Repeat(" ", transportPolicyLimit+1)) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			path, p := policyFixture(t)
			b, _ := json.Marshal(p)
			b = change(b)
			if err := os.WriteFile(path, b, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadTransportPolicy(path); err == nil {
				t.Fatal("invalid file loaded")
			}
			if err := PublishTransportPolicy(path, 0, p); err == nil {
				t.Fatal("invalid file repaired")
			}
			got, _ := os.ReadFile(path)
			if string(got) != string(b) {
				t.Fatal("file changed")
			}
		})
	}
}

func TestTransportPolicyPrivateFilesAndDirectories(t *testing.T) {
	for _, kind := range []string{"public file", "public parent", "file symlink", "parent symlink", "lock symlink", "public lock", "directory file"} {
		t.Run(kind, func(t *testing.T) {
			path, p := policyFixture(t)
			if err := PublishTransportPolicy(path, 0, p); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "public file":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public parent":
				if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
			case "file symlink":
				if err := os.Rename(path, path+".real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+".real", path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				link := filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(filepath.Dir(path), link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, "transport.json")
			case "lock symlink":
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, path+".lock"); err != nil {
					t.Fatal(err)
				}
			case "public lock":
				if err := os.Chmod(path+".lock", 0644); err != nil {
					t.Fatal(err)
				}
			case "directory file":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(kind, "lock") {
				if _, err := LoadTransportPolicy(path); err == nil {
					t.Fatal("unsafe state loaded")
				}
			}
			p.Revision = 2
			if err := PublishTransportPolicy(path, 1, p); err == nil {
				t.Fatal("unsafe state published")
			}
		})
	}
}

func TestTransportPolicyConcurrentCompareAndSwap(t *testing.T) {
	path, p := policyFixture(t)
	if err := PublishTransportPolicy(path, 0, p); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{strings.Repeat("c", 64), strings.Repeat("d", 64)} {
		next := p
		next.Revision = 2
		next.AllowedKeys = append(append([]string{}, p.AllowedKeys...), key)
		wg.Go(func() { <-start; results <- PublishTransportPolicy(path, 1, next) })
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful conflicting updates: %d", successes)
	}
	got, err := LoadTransportPolicy(path)
	if err != nil || got.Revision != 2 || len(got.AllowedKeys) != 3 {
		t.Fatalf("bad committed policy %#v %v", got, err)
	}
}

func TestTransportPolicyRequiresExplicitBootstrapAndCanonicalIdentity(t *testing.T) {
	path, p := policyFixture(t)
	if _, err := LoadTransportPolicy(path); !os.IsNotExist(err) {
		t.Fatalf("missing policy: %v", err)
	}
	later := p
	later.Revision = 2
	if err := PublishTransportPolicy(path, 1, later); err == nil {
		t.Fatal("bootstrapped noninitial revision")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("failed update created policy")
	}
	for _, change := range []func(*TransportPolicy){
		func(p *TransportPolicy) { p.Revision = 0 },
		func(p *TransportPolicy) { p.ClusterID = strings.Repeat("A", 32) },
		func(p *TransportPolicy) { p.CAFingerprint = strings.Repeat("B", 64) },
		func(p *TransportPolicy) { p.RevokedKeys = []string{strings.Repeat("d", 64), strings.Repeat("c", 64)} },
	} {
		invalid := p
		change(&invalid)
		if _, err := invalid.Authorizer(invalid.ClusterID, invalid.CAFingerprint); err == nil {
			t.Fatal("invalid authorization snapshot accepted")
		}
		if err := PublishTransportPolicy(path, 0, invalid); err == nil {
			t.Fatal("invalid bootstrap accepted")
		}
	}
	if err := PublishTransportPolicy(path, ^uint64(0), p); err == nil {
		t.Fatal("overflow revision accepted")
	}
	if err := PublishTransportPolicy(path, 0, p); err != nil {
		t.Fatal(err)
	}
	if err := PublishTransportPolicy(path, 0, p); err != nil {
		t.Fatal("bootstrap confirmation rejected", err)
	}
	for _, name := range []string{path, path + ".lock"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions %o", info.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("publication left temporary files: %v", entries)
	}
}

func TestTransportPolicyLifetimeLock(t *testing.T) {
	path, p := policyFixture(t)
	if err := PublishTransportPolicy(path, 0, p); err != nil {
		t.Fatal(err)
	}
	release, err := LockTransportPolicy(path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	next := p
	next.Revision = 2
	if err := PublishTransportPolicy(path, 1, next); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("live transport publication: %v", err)
	}
	if _, err := LockTransportPolicy(path); !errors.Is(err, filelock.ErrBusy) {
		t.Fatalf("second lifetime owner: %v", err)
	}
	current, err := LoadTransportPolicy(path)
	if err != nil || current.Revision != 1 {
		t.Fatal("live policy changed", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release(); err != nil {
		t.Fatal("release not idempotent", err)
	}
	if err := PublishTransportPolicy(path, 1, next); err != nil {
		t.Fatal("offline update rejected", err)
	}
	restarted, err := LockTransportPolicy(path)
	if err != nil {
		t.Fatal("restart lease", err)
	}
	if err := restarted(); err != nil {
		t.Fatal(err)
	}
}

func TestTransportPolicyLifetimeLockFailureLeavesNoLease(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "public", "linked lock"} {
		t.Run(kind, func(t *testing.T) {
			path, p := policyFixture(t)
			if kind != "missing" {
				if err := PublishTransportPolicy(path, 0, p); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "malformed":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "public":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "linked lock":
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, path+".lock"); err != nil {
					t.Fatal(err)
				}
			}
			release, err := LockTransportPolicy(path)
			if err == nil || release != nil {
				if release != nil {
					_ = release()
				}
				t.Fatal("invalid policy acquired lifetime lease")
			}
			// Explicit test restoration is not automatic production repair. Successful
			// locking afterwards proves the failed attempt retained no descriptor lease.
			if kind == "linked lock" {
				if err := os.Remove(path + ".lock"); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := json.Marshal(p)
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			release, err = LockTransportPolicy(path)
			if err != nil {
				t.Fatal("failed attempt leaked lease", err)
			}
			if err := release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
