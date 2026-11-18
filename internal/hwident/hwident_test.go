package hwident

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// A part is the first 8 bytes of HMAC-SHA256(salt, kind 0x00 raw) in hex
// and the raw value's last four characters; the same inputs give the same
// part, any changed input another digest.
func TestPartDigestsKindAndValueUnderTheSalt(t *testing.T) {
	salt := []byte("acme-2026")
	raw := "4c4c4544-0042-3510-8051-b4c04f4e3432"
	mac := hmac.New(sha256.New, salt)
	mac.Write([]byte("host\x00" + raw))
	want := hex.EncodeToString(mac.Sum(nil))[:16]
	got := Part(salt, KindHost, raw)
	if got.H != want || got.Tail != "3432" || !got.Known() {
		t.Fatalf("Part = %+v, want H %s tail 3432", got, want)
	}
	if again := Part(salt, KindHost, raw); again != got {
		t.Fatalf("same inputs gave %+v then %+v", got, again)
	}
	for name, other := range map[string]IdentityPart{
		"salt":  Part([]byte("acme-2027"), KindHost, raw),
		"kind":  Part(salt, KindMachineID, raw),
		"value": Part(salt, KindHost, raw[:len(raw)-1]+"3"),
	} {
		if other.H == got.H {
			t.Errorf("a different %s gave the same digest", name)
		}
	}
	// The zero byte keeps kind and value apart: "ab"+"c" is not "a"+"bc".
	if Part(salt, "ab", "cdefghijk").H == Part(salt, "a", "bcdefghijk").H {
		t.Fatal("kind and value run together")
	}
}

// The tail is never the whole value: values shorter than eight characters
// have none; it counts characters, not bytes; an empty value is unknown.
func TestPartTail(t *testing.T) {
	salt := []byte("s")
	for raw, tail := range map[string]string{
		"1234567":  "",
		"12345678": "5678",
		"序列号一二三四五": "二三四五",
		"序列号一二三四":  "",
	} {
		if got := Part(salt, KindCard, raw); got.Tail != tail || !got.Known() {
			t.Errorf("Part(%q) = %+v, want tail %q", raw, got, tail)
		}
	}
	if got := Part(salt, KindCard, ""); got.Known() || got != (IdentityPart{}) {
		t.Fatalf("empty raw = %+v", got)
	}
}
