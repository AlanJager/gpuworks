import type { TFunction } from "i18next";
import type { GPU } from "../../api";
import { gpuStateReason } from "../../components/GPUHealth";
import { cardKey, cardOrdinal, cardPickable } from "../../nodes/cardIdentity";

/**
 * The options of a GPU multi-select for one node. Every card is listed so
 * the operator sees the whole node; one that cannot be picked (no device
 * number, or the server says it takes no work) is greyed with the reason.
 * The value is the device index for a card that can be picked and the
 * card's own key otherwise, so cards without a number never share one.
 */
export function gpuSelectOptions(gpus: GPU[] | undefined, tc: TFunction): { value: number | string; label: string; disabled: boolean }[] {
  return (gpus ?? []).map((gpu) => {
    const pickable = cardPickable(gpu);
    const reason = pickable ? "" : gpuStateReason(tc, gpu);
    const base = `GPU ${cardOrdinal(gpu)} · ${gpu.name}`;
    return {
      value: pickable ? gpu.index : cardKey(gpu),
      label: reason ? `${base} · ${reason}` : base,
      disabled: !pickable,
    };
  });
}
