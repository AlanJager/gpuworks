package metrics

import "errors"

// TokenCountError validates the selected same-request counts before their
// context length is formed. A representable input and output can still have an
// unrepresentable sum; rejecting the observation preserves the raw usage for
// diagnostics without publishing a wrapped or fabricated context length.
func (r Request) TokenCountError() error {
	if r.InputTokens < 0 || r.OutputTokens < 0 {
		return errors.New("invalid token usage: input and output counts must be nonnegative")
	}
	if r.InputTokens > int(^uint(0)>>1)-r.OutputTokens {
		return errors.New("invalid token usage: input plus output exceeds the supported integer range")
	}
	return nil
}

func addTokenCount(total int64, value int) (int64, bool) {
	if total < 0 || value < 0 || int64(value) > int64(^uint64(0)>>1)-total {
		return 0, false
	}
	return total + int64(value), true
}
