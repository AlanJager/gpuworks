package tokenizer

import (
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"strings"
)

type Tokenizer struct {
	vocab         map[string]int
	tokens        []string
	charsPerToken float64
	byteDecoder   map[rune]byte
}

func Load(r io.Reader) (*Tokenizer, error) {
	var doc struct {
		Model struct {
			Vocab map[string]int `json:"vocab"`
		} `json:"model"`
		Vocab   map[string]int `json:"vocab"`
		Decoder struct {
			Type string `json:"type"`
		} `json:"decoder"`
	}
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, err
	}
	v := doc.Model.Vocab
	if len(v) == 0 {
		v = doc.Vocab
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("tokenizer.json has no BPE vocab")
	}
	tokens := make([]string, 0, len(v))
	for token := range v {
		if token != "" && !strings.HasPrefix(token, "<") {
			tokens = append(tokens, token)
		}
	}
	sort.Strings(tokens)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("tokenizer.json has no sampleable tokens")
	}
	var decoder map[rune]byte
	if doc.Decoder.Type == "ByteLevel" {
		decoder = newByteLevelDecoder()
	}
	return &Tokenizer{vocab: v, tokens: tokens, charsPerToken: 4, byteDecoder: decoder}, nil
}

// Sample returns text decoded from n independently sampled tokenizer
// vocabulary entries. ByteLevel vocabularies must go through the tokenizer's
// byte decoder; treating markers such as Ġ as literal text can expand one
// sampled entry into several server-side tokens.
func (t *Tokenizer) Sample(r *rand.Rand, n int) string {
	if t == nil || len(t.tokens) == 0 || n <= 0 {
		return ""
	}
	if t.byteDecoder != nil {
		var encoded strings.Builder
		for i := 0; i < n; i++ {
			encoded.WriteString(t.tokens[r.Intn(len(t.tokens))])
		}
		return decodeByteLevel(encoded.String(), t.byteDecoder)
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		tok := t.tokens[r.Intn(len(t.tokens))]
		space := strings.HasPrefix(tok, "Ġ") || strings.HasPrefix(tok, "▁")
		tok = strings.TrimPrefix(strings.TrimPrefix(tok, "Ġ"), "▁")
		if tok == "" {
			tok = "x"
		}
		if i > 0 && (space || needsSpace(tok)) {
			b.WriteByte(' ')
		}
		b.WriteString(tok)
	}
	return b.String()
}

func newByteLevelDecoder() map[rune]byte {
	used := make([]bool, 256)
	bytesInOrder := make([]int, 0, 256)
	for b := int('!'); b <= int('~'); b++ {
		bytesInOrder = append(bytesInOrder, b)
		used[b] = true
	}
	for b := 0xA1; b <= 0xAC; b++ {
		bytesInOrder = append(bytesInOrder, b)
		used[b] = true
	}
	for b := 0xAE; b <= 0xFF; b++ {
		bytesInOrder = append(bytesInOrder, b)
		used[b] = true
	}
	codepoints := append([]int(nil), bytesInOrder...)
	n := 0
	for b, present := range used {
		if !present {
			bytesInOrder = append(bytesInOrder, b)
			codepoints = append(codepoints, 256+n)
			n++
		}
	}
	decoder := make(map[rune]byte, 256)
	for i, codepoint := range codepoints {
		decoder[rune(codepoint)] = byte(bytesInOrder[i])
	}
	return decoder
}

func decodeByteLevel(encoded string, decoder map[rune]byte) string {
	decoded := make([]byte, 0, len(encoded))
	for _, char := range encoded {
		if b, ok := decoder[char]; ok {
			decoded = append(decoded, b)
			continue
		}
		decoded = append(decoded, string(char)...)
	}
	return strings.ToValidUTF8(string(decoded), "�")
}

func needsSpace(token string) bool {
	r := []rune(token)
	if len(r) == 0 {
		return false
	}
	return (r[0] >= 'A' && r[0] <= 'Z') || (r[0] >= 'a' && r[0] <= 'z') || (r[0] >= '0' && r[0] <= '9')
}
func (t *Tokenizer) Count(s string) int {
	if t == nil {
		return Estimate(s, 4)
	}
	n := 0
	for _, w := range strings.Fields(s) {
		if id, ok := t.vocab[w]; ok {
			_ = id
			n++
		} else {
			n += max(1, (len([]rune(w))+2)/3)
		}
	}
	return n
}
func Estimate(s string, charsPerToken float64) int {
	if charsPerToken <= 0 {
		charsPerToken = 4
	}
	return max(1, int(float64(len([]rune(s)))/charsPerToken+0.5))
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type Calibrator struct {
	target   int
	observed int
	chars    float64
}

func NewCalibrator(target int) *Calibrator { return &Calibrator{target: target, chars: 4} }
func (c *Calibrator) Observe(chars, tokens int) {
	if chars > 0 && tokens > 0 {
		c.chars = float64(chars) / float64(tokens)
		c.observed = tokens
	}
}
func (c *Calibrator) Estimate(s string) int { return Estimate(s, c.chars) }
