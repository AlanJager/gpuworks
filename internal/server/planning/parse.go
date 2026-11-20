package planning

import (
	"encoding/json"
	"strings"
)

// Small tolerant parsers for the stored text planning reads: a compute
// capability ("8.0") and a JSON integer list. A value that does not parse
// reads as zero or none.

func parseCC(s string) float64 {
	var maj, min int
	if _, err := fscan(s, &maj, &min); err != nil {
		return 0
	}
	return float64(maj) + float64(min)/10
}

func fscan(s string, maj, min *int) (int, error) {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) == 0 {
		return 0, errParse
	}
	*maj = atoi(parts[0])
	if len(parts) == 2 {
		*min = atoi(parts[1])
	}
	return 2, nil
}

var errParse = errStr("parse")

type errStr string

func (e errStr) Error() string { return string(e) }

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func parseInts(s string) []int {
	if s == "" {
		return nil
	}
	var v []int
	_ = json.Unmarshal([]byte(s), &v)
	return v
}
