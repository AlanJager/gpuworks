package api

import (
	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/cardhealth"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func cardHealthNotice(cards []store.GPU, selected string, snap cardhealth.NodeSnapshot, view cardhealth.View, supported bool) *dto.CardHealthNotice {
	notice := func(code string) *dto.CardHealthNotice {
		return &dto.CardHealthNotice{Code: code, Level: "info"}
	}
	switch {
	case !snap.Online:
		return &dto.CardHealthNotice{Code: messages.CardChecksOffline, Level: "warning"}
	case !supported:
		return notice(messages.ErrorCardChecksUnsupported)
	case snap.ReceivedAt.IsZero():
		return notice(messages.CardChecksWaiting)
	case view.Stale:
		return &dto.CardHealthNotice{Code: messages.CardChecksStale, Level: "warning"}
	}

	count, usable := 0, 0
	ownership := ""
	for _, card := range cards {
		if selected != "" && accelerator.CardIdent(card.UUID, card.PCIeBDF) != selected {
			continue
		}
		count++
		if accelerator.CardUsable(card.Ownership) {
			usable++
		}
		if count == 1 {
			ownership = card.Ownership
		} else if ownership != card.Ownership {
			ownership = "mixed"
		}
	}
	if count == 0 {
		return notice(messages.CardChecksNoCards)
	}
	if usable == 0 {
		switch ownership {
		case accelerator.CardPassedThrough:
			return notice(messages.CardChecksPassthrough)
		case accelerator.CardWithoutDriver:
			return notice(messages.CardChecksNoDriver)
		case accelerator.CardHeldElsewhere:
			return notice(messages.CardChecksHeldElsewhere)
		default:
			return notice(messages.CardChecksNoUsableCards)
		}
	}
	for _, card := range snap.Cards {
		if len(card.Checks) > 0 {
			return nil
		}
	}
	return notice(messages.CardChecksWaiting)
}
