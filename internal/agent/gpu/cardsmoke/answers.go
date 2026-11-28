package cardsmoke

import (
	"errors"
	"strings"
)

// Failures of an attempt that are not the card's answer.

// Inconclusive marks an attempt's failure as saying nothing about the card:
// the attempt never reached it. The card is then left exactly as it was, and
// the round stops because whatever stopped the attempt would stop every other
// one in it too.
func Inconclusive(reason string) error {
	return inconclusiveError{reason: strings.TrimSpace(reason)}
}

type inconclusiveError struct{ reason string }

func (e inconclusiveError) Error() string { return e.reason }

// NotHeld marks an attempt's failure as "the platform does not hold this
// card": it is not the platform's to ask (on a Kubernetes node, the kubelet may
// give it to another pod at any moment). The attempt never touched the card, so
// nothing is recorded about it except that it was skipped, and — unlike
// Inconclusive — the round goes on with the other cards.
func NotHeld(reason string) error {
	return notHeldError{reason: strings.TrimSpace(reason)}
}

type notHeldError struct{ reason string }

func (e notHeldError) Error() string {
	if e.reason == "" {
		return "the platform does not hold this card"
	}
	return e.reason
}

// NotHeldReason is why the platform does not hold the card the attempt was
// about, or "" for any other error.
func NotHeldReason(err error) string {
	var held notHeldError
	if errors.As(err, &held) {
		return held.Error()
	}
	return ""
}

// Queued marks an attempt that never started: another check on the node held
// the runtime for all the time the attempt was given. Its deadline passed, but
// the card was not asked, so it is not the card's timeout. Nothing is
// recorded about the card, which is skipped as SkipInProgress, and the round
// goes on: the check that held the runtime ends on its own.
func Queued(reason string) error {
	return queuedError{reason: strings.TrimSpace(reason)}
}

type queuedError struct{ reason string }

func (e queuedError) Error() string {
	if e.reason == "" {
		return "another runtime check held the runtime for all the time this one had"
	}
	return e.reason
}

func queued(err error) bool {
	var q queuedError
	return errors.As(err, &q)
}

func inconclusiveReason(err error) string {
	var inconclusive inconclusiveError
	if errors.As(err, &inconclusive) {
		if inconclusive.reason == "" {
			return "the runtime check could not be made"
		}
		return inconclusive.reason
	}
	return ""
}
