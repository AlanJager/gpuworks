package proxy

import (
	"bufio"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func affinityKey(r *http.Request) string {
	if key := strings.TrimSpace(r.Header.Get("X-GPUWorks-Affinity-Key")); key != "" {
		r.Header.Del("X-GPUWorks-Affinity-Key")
		return key
	}
	if r.GetBody == nil {
		return ""
	}
	body, err := r.GetBody()
	if err != nil {
		return ""
	}
	defer body.Close()
	return affinityKeyJSON(body)
}

func affinityKeyJSON(source io.Reader) string {
	dec := json.NewDecoder(bufio.NewReader(source))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ""
	}
	var fallback string
	for dec.More() {
		keyToken, err := dec.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok {
			return ""
		}
		switch key {
		case "messages":
			if value, ok := affinityMessages(dec); ok {
				return value
			}
		case "prompt", "input":
			var raw json.RawMessage
			if dec.Decode(&raw) != nil {
				return ""
			}
			if len(raw) > 4<<10 {
				raw = raw[:4<<10]
			}
			if fallback == "" {
				fallback = string(raw)
			}
		default:
			if skipJSONValue(dec) != nil {
				return ""
			}
		}
	}
	return fallback
}

func affinityMessages(dec *json.Decoder) (string, bool) {
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('[') {
		return "", false
	}
	var out []byte
	for dec.More() {
		var item json.RawMessage
		if err := dec.Decode(&item); err != nil {
			return "", false
		}
		out = append(out, item...)
		var role struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(item, &role)
		if strings.EqualFold(role.Role, "user") || len(out) >= 64<<10 {
			if len(out) > 64<<10 {
				out = out[:64<<10]
			}
			return string(out), true
		}
	}
	return string(out), true
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	for dec.More() {
		if delim == '{' {
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		if err := skipJSONValue(dec); err != nil {
			return err
		}
	}
	_, err = dec.Token()
	return err
}

func fnv64(data []byte) uint64 { h := fnv.New64a(); _, _ = h.Write(data); return h.Sum64() }
func mix64(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func (b *balancer) pickAffinity(candidates []candidate, key string, excluded map[string]bool, online func(string) bool) *lease {
	if strings.TrimSpace(key) == "" {
		return b.pick(candidates, excluded, online)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	nowFn := b.now
	if nowFn == nil {
		nowFn = time.Now
	}
	type scored struct {
		c     candidate
		score uint64
	}
	items := make([]scored, 0, len(candidates))
	total := 0
	for _, c := range candidates {
		if excluded[c.instanceUUID] || (online != nil && !online(c.nodeUUID)) {
			continue
		}
		s := b.state(c.instanceUUID)
		if s.cooldownUntil.IsZero() || nowFn().After(s.cooldownUntil) {
			total += s.inflight
		}
		uuidHash := c.uuidHash
		if uuidHash == 0 {
			uuidHash = fnv64([]byte(c.instanceUUID))
		}
		items = append(items, scored{c, mix64(fnv64([]byte(key)) ^ uuidHash)})
	}
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].score > items[j].score })
	avg := float64(total+1) / float64(len(items))
	limit := int(avg * 1.5)
	if limit < 4 {
		limit = 4
	}
	for _, item := range items {
		s := b.state(item.c.instanceUUID)
		if (s.cooldownUntil.IsZero() || nowFn().After(s.cooldownUntil)) && s.inflight < limit {
			s.inflight++
			l := &lease{c: item.c}
			l.releaseFn = func(o attemptOutcome) { b.release(l, o) }
			return l
		}
	}
	best := items[0]
	for _, item := range items[1:] {
		if b.state(item.c.instanceUUID).inflight < b.state(best.c.instanceUUID).inflight {
			best = item
		}
	}
	s := b.state(best.c.instanceUUID)
	s.inflight++
	l := &lease{c: best.c}
	l.releaseFn = func(o attemptOutcome) { b.release(l, o) }
	return l
}
