package app

import "testing"

func TestModelArchiveLimit(t *testing.T) {
	t.Run("small archive keeps headroom", func(t *testing.T) {
		const expected int64 = 100 << 30
		want := expected + expected/20 + 1<<30
		if got := modelArchiveLimit(expected); got != want {
			t.Fatalf("limit = %d, want %d", got, want)
		}
	})
	t.Run("Kimi K3.1 Pro fits", func(t *testing.T) {
		const expected int64 = 1466844769783
		got := modelArchiveLimit(expected)
		if got <= expected {
			t.Fatalf("limit = %d, expected archive bytes = %d", got, expected)
		}
		if want := expected + expected/20 + 1<<30; got != want {
			t.Fatalf("limit = %d, want expected plus headroom = %d", got, want)
		}
	})
}
