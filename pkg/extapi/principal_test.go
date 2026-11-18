package extapi

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// principalVectorKey is the key of the signed principal vectors: bytes
// 0..31, the principal_key of the handshake golden files.
func principalVectorKey() []byte {
	k := make([]byte, PrincipalKeyBytes)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// principalVectorNow is a moment every vector is valid at.
var principalVectorNow = time.Date(2026, 12, 1, 0, 0, 5, 0, time.UTC)

// A signed vector principal/v1/signed/<name>.txt holds the three headers
// the server sets for principal/v1/valid/<name>.json under
// principalVectorKey. The vectors are frozen like the rest of the corpus,
// so the encoding and the MAC input (principal header, "\n", feature
// access) cannot change.
func TestSignedPrincipalVectorsMatchSignPrincipal(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(goldenRoot, "principal", "v1", "signed", "*.txt"))
	if len(files) == 0 {
		t.Fatal("no signed principal vectors")
	}
	listed := readGoldenSums(t)
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			rel, _ := filepath.Rel(goldenRoot, file)
			if _, ok := listed[filepath.ToSlash(rel)]; !ok {
				raw, _ := os.ReadFile(file)
				t.Errorf("%s is not in %s; append: %s  %s", file, goldenSums, sha256Hex(raw), filepath.ToSlash(rel))
			}
			h := readSignedVector(t, file)
			name := strings.TrimSuffix(filepath.Base(file), ".txt")
			raw, err := os.ReadFile(filepath.Join(goldenRoot, "principal", "v1", "valid", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			p, err := DecodePrincipal(raw)
			if err != nil {
				t.Fatal(err)
			}
			hdr, mac, err := SignPrincipal(principalVectorKey(), p, h[HeaderFeatureAccess])
			if err != nil {
				t.Fatal(err)
			}
			if hdr != h[HeaderPrincipal] || mac != h[HeaderPrincipalMac] {
				t.Fatalf("SignPrincipal gives\n%s: %s\n%s: %s\nthe vector has\n%s: %s\n%s: %s",
					HeaderPrincipal, hdr, HeaderPrincipalMac, mac, HeaderPrincipal, h[HeaderPrincipal], HeaderPrincipalMac, h[HeaderPrincipalMac])
			}
			got, access, err := VerifyPrincipal(principalVectorKey(), hdr, mac, h[HeaderFeatureAccess], principalVectorNow)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := Marshal(got)
			if !bytes.Equal(again, raw) || access.String() != h[HeaderFeatureAccess] {
				t.Fatalf("verified %s with %q; want %s with %q", again, access, raw, h[HeaderFeatureAccess])
			}
		})
	}
}

// readSignedVector reads "Name: value" lines; an empty value is written
// "Name:" so the file has no trailing space.
func readSignedVector(t *testing.T, file string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s: malformed line %q", file, line)
		}
		out[name] = strings.TrimPrefix(value, " ")
	}
	for _, name := range []string{HeaderFeatureAccess, HeaderPrincipal, HeaderPrincipalMac} {
		if _, ok := out[name]; !ok || len(out) != 3 {
			t.Fatalf("%s: want exactly the lines %s, %s and %s", file, HeaderFeatureAccess, HeaderPrincipal, HeaderPrincipalMac)
		}
	}
	return out
}

func adminPrincipal() *Principal {
	return &Principal{Kind: PrincipalSession, UserID: 1, Username: "admin", PlatformAdmin: true, LocalAdmin: true,
		Roles: []string{"admin"}, RequestID: "req-1", RouteAction: "acme_reports.read", Route: "/reports/{period}",
		Exp: PrincipalExpiry(principalVectorNow, 0)}
}

func TestVerifyPrincipalRefusesWhatTheServerDidNotSign(t *testing.T) {
	key := principalVectorKey()
	hdr, mac, err := SignPrincipal(key, adminPrincipal(), "acme.reports=read_only")
	if err != nil {
		t.Fatal(err)
	}
	previous := bytes.Repeat([]byte{9}, PrincipalKeyBytes) // the key of an earlier spawn
	oldHdr, oldMac, err := SignPrincipal(previous, adminPrincipal(), "acme.reports=read_only")
	if err != nil {
		t.Fatal(err)
	}
	// A principal the right key signed, but with an extra field: only a
	// broken server would send it, and it must still be refused.
	unknownRaw := []byte(`{"delegation":"x","exp":"2026-12-01T00:01:10Z","kind":"public","request_id":"r","route":"/a","route_action":"acme_sso.acs"}`)
	unknownHdr := b64url.EncodeToString(unknownRaw)
	unknownMac := b64url.EncodeToString(principalMAC(key, unknownHdr, ""))
	spacedRaw := []byte(`{"exp":"2026-12-01T00:01:10Z", "kind":"public","request_id":"r","route":"/a","route_action":"acme_sso.acs"}`)
	spacedHdr := b64url.EncodeToString(spacedRaw)
	spacedMac := b64url.EncodeToString(principalMAC(key, spacedHdr, ""))
	paddedHdr := base64.URLEncoding.EncodeToString(adminJSON(t))
	if paddedHdr == hdr {
		t.Fatal("the admin principal needs no padding; change its length")
	}
	paddedMac := b64url.EncodeToString(principalMAC(key, paddedHdr, "acme.reports=read_only"))
	unsortedMac := b64url.EncodeToString(principalMAC(key, hdr, "acme.zeta=full,acme.alpha=full"))

	cases := []struct {
		name             string
		key              []byte
		hdr, mac, access string
		now              time.Time
		want             error
		code             string
	}{
		{name: "wrong MAC", key: key, hdr: hdr, mac: b64url.EncodeToString(make([]byte, 32)), access: "acme.reports=read_only", want: ErrPrincipalMAC},
		{name: "MAC not base64url", key: key, hdr: hdr, mac: mac + "=", access: "acme.reports=read_only", want: ErrPrincipalMAC},
		{name: "the previous spawn's key", key: key, hdr: oldHdr, mac: oldMac, access: "acme.reports=read_only", want: ErrPrincipalMAC},
		{name: "tampered feature access", key: key, hdr: hdr, mac: mac, access: "acme.reports=full", want: ErrPrincipalMAC},
		{name: "feature access dropped", key: key, hdr: hdr, mac: mac, access: "", want: ErrPrincipalMAC},
		{name: "tampered principal", key: key, hdr: tamper(hdr), mac: mac, access: "acme.reports=read_only", want: ErrPrincipalMAC},
		{name: "missing principal", key: key, hdr: "", mac: mac, access: "acme.reports=read_only", want: ErrPrincipalMissing},
		{name: "missing MAC", key: key, hdr: hdr, mac: "", access: "acme.reports=read_only", want: ErrPrincipalMissing},
		{name: "short key", key: key[:16], hdr: hdr, mac: mac, access: "acme.reports=read_only", want: ErrPrincipalMAC},
		{name: "expired", key: key, hdr: hdr, mac: mac, access: "acme.reports=read_only", now: principalVectorNow.Add(66 * time.Second), want: ErrPrincipalExpired},
		{name: "expiry beyond any route", key: key, hdr: hdr, mac: mac, access: "acme.reports=read_only", now: principalVectorNow.Add(-time.Hour), want: ErrPrincipalTooLong},
		{name: "unknown field", key: key, hdr: unknownHdr, mac: unknownMac, code: ErrorWireUnknownField},
		{name: "not canonical", key: key, hdr: spacedHdr, mac: spacedMac, code: ErrorWireNotCanonical},
		{name: "padded base64", key: key, hdr: paddedHdr, mac: paddedMac, access: "acme.reports=read_only", code: ErrorWireMalformed},
		{name: "unsorted feature access", key: key, hdr: hdr, mac: unsortedMac, access: "acme.zeta=full,acme.alpha=full", code: ErrorWireInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := c.now
			if now.IsZero() {
				now = principalVectorNow
			}
			p, _, err := VerifyPrincipal(c.key, c.hdr, c.mac, c.access, now)
			if p != nil || err == nil {
				t.Fatalf("verified %+v", p)
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Fatalf("error %v, want %v", err, c.want)
			}
			if c.code != "" && CodeOf(err) != c.code {
				t.Fatalf("error %v (%s), want %s", err, CodeOf(err), c.code)
			}
		})
	}
	if _, _, err := VerifyPrincipal(key, hdr, mac, "acme.reports=read_only", principalVectorNow.Add(65*time.Second)); err != nil {
		t.Fatalf("at exp itself: %v", err)
	}
}

func adminJSON(t *testing.T) []byte {
	raw, err := Marshal(adminPrincipal())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func tamper(hdr string) string {
	raw, _ := b64url.DecodeString(hdr)
	raw = bytes.Replace(raw, []byte(`"user_id":1`), []byte(`"user_id":2`), 1)
	return b64url.EncodeToString(raw)
}

func TestSignPrincipalRefusesWhatItCannotSign(t *testing.T) {
	key := principalVectorKey()
	bad := adminPrincipal()
	bad.Kind = PrincipalToken // a token never carries local_admin
	if _, _, err := SignPrincipal(key, bad, ""); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("signed an invalid principal: %v", err)
	}
	if _, _, err := SignPrincipal(key, adminPrincipal(), "acme.reports=hidden"); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("signed a hidden feature: %v", err)
	}
	if _, _, err := SignPrincipal(key[:31], adminPrincipal(), ""); err == nil {
		t.Fatal("signed with a short key")
	}
}

func TestFeatureAccessHasOneSpelling(t *testing.T) {
	good := map[string]FeatureAccess{
		"":                 {},
		"acme.backup=full": {"acme.backup": AccessFull},
		"acme.backup=read_only,acme.export=locked": {"acme.backup": AccessReadOnly, "acme.export": AccessLocked},
	}
	for s, want := range good {
		got, err := ParseFeatureAccess(s)
		if err != nil || got.String() != s || len(got) != len(want) {
			t.Fatalf("%q: %v %v", s, got, err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("%q: %s=%s, want %s", s, k, got[k], v)
			}
		}
		if err := want.Validate(); err != nil || want.String() != s {
			t.Fatalf("%v formats as %q (%v), want %q", want, want.String(), err, s)
		}
	}
	bad := map[string]string{
		"acme.backup":                         "0:grammar",
		"acme.backup=hidden":                  "0:enum",
		"acme.backup=":                        "0:required",
		"acme.backup=full,":                   "1:grammar",
		" acme.backup=full":                   "0:grammar",
		"acme.backup=full, acme.export=full":  "1:grammar",
		"acme.export=full,acme.backup=full":   "1:order",
		"acme.export=full,acme.export=locked": "1:duplicate",
		"Acme.x=full":                         "0:grammar",
		"acme.backup=FULL":                    "0:enum",
		"acme.backup=full;acme.export=locked": "0:enum",
	}
	for s, want := range bad {
		_, err := ParseFeatureAccess(s)
		var e *Error
		if !errors.As(err, &e) || e.Code != ErrorWireInvalid {
			t.Fatalf("%q: %v", s, err)
		}
		if got := e.Params["field"].(string) + ":" + e.Params["rule"].(string); got != want {
			t.Errorf("%q: %s, want %s", s, got, want)
		}
	}
	if err := (FeatureAccess{"acme.export": AccessHidden}).Validate(); CodeOf(err) != ErrorWireInvalid {
		t.Fatalf("hidden validated: %v", err)
	}
}

func TestPrincipalExpiryIsTheRouteTimeoutPlusSlack(t *testing.T) {
	at := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		now      time.Time
		timeoutS int
		want     string
	}{
		{at, 0, "2026-12-01T00:01:05Z"},
		{at, 30, "2026-12-01T00:00:35Z"},
		{at, MaxRouteTimeoutS, "2026-12-01T00:10:05Z"},
		{at.Add(300 * time.Millisecond), 30, "2026-12-01T00:00:36Z"}, // rounded up, never shortened
	}
	for _, c := range cases {
		if got := PrincipalExpiry(c.now, c.timeoutS); got.Format(time.RFC3339) != c.want {
			t.Errorf("%s +%ds: %s, want %s", c.now, c.timeoutS, got.Format(time.RFC3339), c.want)
		}
	}
}
