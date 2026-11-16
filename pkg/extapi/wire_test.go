package extapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCanonicalForm(t *testing.T) {
	cases := map[string]string{
		`{"b":1,"a":[true,false,null],"c":{"z":"","y":-0}}`: `{"a":[true,false,null],"b":1,"c":{"y":0,"z":""}}`,
		"{ \"a\" : 1 }":                `{"a":1}`,
		`"Aé中\/"`:                      `"Aé中/"`,
		`"\b\f\n\r\t\u0001\u001f\"\\"`: `"\b\f\n\r\t\u0001\u001f\"\\"`,
		`"<>&` + " " + `"`:             `"<>&` + " " + `"`,
		`{"é":1,"z":2,"Z":3}`:          `{"Z":3,"z":2,"é":1}`,
		`9007199254740991`:             `9007199254740991`,
		`-9007199254740991`:            `-9007199254740991`,
		`[]`:                           `[]`,
		`{}`:                           `{}`,
	}
	for in, want := range cases {
		got, err := Canonical([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if string(got) != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
		if !IsCanonical(got) {
			t.Errorf("%s: canonical output is not canonical", in)
		}
	}
}

func TestCanonicalRefusals(t *testing.T) {
	for in, rule := range map[string]string{
		`{"a":1,"a":2}`:    RuleDuplicate,
		`1.0`:              RuleNumber,
		`1e3`:              RuleNumber,
		`9007199254740992`: RuleNumber,
		`{"a":1} {}`:       RuleTrailing,
		`{"a":`:            RuleSyntax,
		"\"\xff\"":         RuleSyntax,
		strings.Repeat("[", 70) + strings.Repeat("]", 70): RuleRange,
	} {
		_, err := Canonical([]byte(in))
		e, ok := err.(*Error)
		if !ok || e.Code != ErrorWireMalformed || e.Params["rule"] != rule {
			t.Errorf("%q: %v, want %s", in, err, rule)
		}
	}
}

func TestDigestIsOverTheCanonicalForm(t *testing.T) {
	a, err := DigestValue(Notice{Code: "ACME_X", Params: Params{"b": 1, "a": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := Canonical([]byte(`{"params":{"a":"x","b":1},"code":"ACME_X"}`))
	if a != DigestOf(c) || !a.Valid() {
		t.Fatalf("digest %s, want %s", a, DigestOf(c))
	}
}

func TestStrictDecodeIsCaseSensitive(t *testing.T) {
	// encoding/json alone would match "Code" to the field "code".
	_, err := DecodeWireError([]byte(`{"Code":"ACME_X","message":"m","reason":"internal"}`))
	if e, ok := err.(*Error); !ok || e.Code != ErrorWireUnknownField || e.Params["field"] != "Code" {
		t.Fatalf("got %v", err)
	}
}

func TestDisplayIsLenientAndKept(t *testing.T) {
	body := `{"rows":[{"display":{"from_a_newer_build":{"nested":[1,"x"]}},"down_access":"locked","feature":"acme.backup","steps":[{"access":"full"}]}]}`
	tbl, err := DecodeTable(KindAvailability, 1, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	out, err := Marshal(tbl)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != body {
		t.Fatalf("display not kept:\n%s", out)
	}
	if _, err := DecodeTable(KindAvailability, 1, []byte(strings.Replace(body, `"display":{`, `"display":[`, 1))); CodeOf(err) != ErrorWireMalformed {
		t.Fatalf("display that is not an object: %v", err)
	}
}

func TestTimeHasOneSpelling(t *testing.T) {
	var tm Time
	if err := json.Unmarshal([]byte(`"2026-11-17T00:00:00Z"`), &tm); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"2026-11-17T08:00:00+08:00"`, `"2026-11-17T00:00:00.000Z"`, `"2026-11-17 00:00:00Z"`, `"2026-11-17"`, `17`} {
		if err := json.Unmarshal([]byte(bad), &tm); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	local := Time{time.Date(2026, 1, 1, 8, 0, 0, 0, time.FixedZone("x", 8*3600))}
	if _, err := json.Marshal(local); err == nil {
		t.Error("non-UTC time encoded")
	}
	if b, err := json.Marshal(NewTime(local.Time.Add(time.Millisecond))); err != nil || string(b) != `"2026-01-01T00:00:00Z"` {
		t.Errorf("NewTime: %s %v", b, err)
	}
}

func TestParamsKeepIntegers(t *testing.T) {
	h, err := DecodeHealth([]byte(`{"code":"ACME_X","params":{"n":9007199254740991},"status":"degraded"}`))
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := h.Params["n"].(json.Number); !ok || n.String() != "9007199254740991" {
		t.Fatalf("params %#v", h.Params)
	}
	if f := (Params{"n": int64(1) << 60}).validate("p"); f == nil || f.rule != RuleNumber {
		t.Fatalf("unsafe int64 param: %v", f)
	}
	if f := (Params{"n": 1.5}).validate("p"); f == nil || f.rule != RuleType {
		t.Fatalf("float param: %v", f)
	}
}

func TestStateTooLarge(t *testing.T) {
	big := `{"attestations":[{"blob":"` + strings.Repeat("A", (MaxAttestationBlob+3)/3*4+4) + `","format":"f","name":"n"}]}`
	if _, err := DecodeTable(KindAttestations, 1, []byte(big)); CodeOf(err) != ErrorStateTooLarge {
		t.Fatalf("oversized blob: %v", err)
	}
	if _, err := DecodeStatePush(make([]byte, MaxPushBytes+1)); CodeOf(err) != ErrorStateTooLarge {
		t.Fatalf("oversized push: %v", err)
	}
}

func TestUnknownTableIsTooNew(t *testing.T) {
	p, err := DecodeStatePush([]byte(`{"body":{},"generation":3,"kind":"quota_limits","schema":1}`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.DecodeBody()
	if e, ok := err.(*Error); !ok || e.Code != ErrorSchemaTooNew || e.Params["object"] != "table.quota_limits" || e.Params["known"] != 0 {
		t.Fatalf("unknown kind: %v", err)
	}
	_, err = DecodeTable(KindAvailability, 2, []byte(`{}`))
	if e, ok := err.(*Error); !ok || e.Code != ErrorSchemaTooNew || e.Params["known"] != 1 {
		t.Fatalf("newer schema: %v", err)
	}
}

func TestWireErrorOf(t *testing.T) {
	_, err := DecodeTable(KindNodeHolds, 1, []byte(`{"nodes":{"x":{"code":"ACME_X"}}}`))
	e, ok := err.(*Error)
	if !ok || e.Params["row"] != "x" {
		t.Fatalf("%v", err)
	}
	w := WireErrorOf(e, ReasonInvalidArgument)
	raw, err := Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWireError(raw); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
}
