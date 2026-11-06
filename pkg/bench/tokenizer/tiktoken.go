package tokenizer

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// TiktokenFile is the vocabulary file tiktoken-based models ship instead of
// tokenizer.json (Kimi, early Qwen): one "<base64 token bytes> <rank>" line
// per token.
const TiktokenFile = "tiktoken.model"

// LoadTiktoken builds a tokenizer from a tiktoken vocabulary. Prompts are
// sampled from its entries, so only those that are whole UTF-8 text are
// kept: a byte-level entry that ends inside a character would sample as a
// replacement character. Special tokens are not in the file.
func LoadTiktoken(r io.Reader) (*Tokenizer, error) {
	vocab := map[string]int{}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64<<10), 1<<20)
	lines := 0
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		lines++
		encoded, rank, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("tiktoken line %d is not \"<base64> <rank>\"", lines)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("tiktoken line %d: %w", lines, err)
		}
		id, err := strconv.Atoi(strings.TrimSpace(rank))
		if err != nil {
			return nil, fmt.Errorf("tiktoken line %d: rank: %w", lines, err)
		}
		if text := string(raw); text != "" && utf8.ValidString(text) {
			vocab[text] = id
		}
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	if lines == 0 {
		return nil, fmt.Errorf("tiktoken vocabulary is empty")
	}
	return fromDecodedVocab(vocab)
}

// fromDecodedVocab builds a tokenizer whose vocabulary entries are already
// the text they stand for (tiktoken bytes, or what an engine detokenized),
// so sampling joins them as they are.
func fromDecodedVocab(vocab map[string]int) (*Tokenizer, error) {
	tokens := make([]string, 0, len(vocab))
	for token := range vocab {
		if token != "" && !strings.HasPrefix(token, "<") {
			tokens = append(tokens, token)
		}
	}
	sort.Strings(tokens)
	if len(tokens) == 0 {
		return nil, fmt.Errorf("vocabulary has no sampleable tokens")
	}
	return &Tokenizer{vocab: vocab, tokens: tokens, charsPerToken: 4, decoded: true}, nil
}

// LoadFile reads a tokenizer.json or a tiktoken vocabulary, telling them
// apart by content: a tokenizer.json is a JSON object.
func LoadFile(r io.Reader) (*Tokenizer, error) {
	head := bufio.NewReader(r)
	for {
		b, err := head.Peek(1)
		if err != nil {
			return nil, fmt.Errorf("tokenizer file is empty")
		}
		if bytes.ContainsAny(b, " \t\r\n") {
			_, _ = head.ReadByte()
			continue
		}
		if b[0] == '{' {
			return Load(head)
		}
		return LoadTiktoken(head)
	}
}
