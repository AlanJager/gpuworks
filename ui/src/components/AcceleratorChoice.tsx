import { vendorName } from "../vendorName";
import { ExclamationCircleFilled, InfoCircleFilled } from "@ant-design/icons";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type AcceleratorPresence, type Node } from "../api";
import { message } from "../ui-kit/feedback";
import { Select, SelectContent, SelectItem, SelectTrigger } from "./ui/select";

type AcceleratorNode = Pick<Node, "uuid" | "gpu_vendor" | "accelerator_preference" | "accelerator_vendors">;

/**
 * Whether the node's host has cards of more than one vendor, which is the
 * only case where choosing which of them the node manages means anything.
 */
export function hasAcceleratorChoice(node: Pick<Node, "accelerator_vendors">): boolean {
  return (node.accelerator_vendors?.filter((v) => v.cards > 0).length ?? 0) > 1;
}

function useVendorName() {
  const { t } = useTranslation("common");
  return (vendor?: string) => vendorName(vendor, t);
}

/**
 * The choice of which vendor's cards a node manages, on a host with cards of
 * more than one. Automatic keeps the agent's own pick and says which other
 * cards are left unmanaged; switching restarts the agent onto the other
 * vendor's cards, which the server refuses while instances run.
 */
export default function AcceleratorChoice({ node, onUpdated }: { node: AcceleratorNode; onUpdated?: (node: Node) => void }) {
  const { t } = useTranslation("nodes");
  const vendorName = useVendorName();
  const [loading, setLoading] = useState(false);
  if (!hasAcceleratorChoice(node)) return null;

  const vendors = (node.accelerator_vendors ?? []).filter((v) => v.cards > 0);
  const managed = (node.gpu_vendor ?? "").toLowerCase();
  const preference = node.accelerator_preference || "auto";
  const describe = (v: AcceleratorPresence) =>
    t(v.model ? "accelerator.cardsWithModel" : "accelerator.cards", { vendor: vendorName(v.vendor), n: v.cards, model: v.model });
  const unmanaged = vendors.filter((v) => v.vendor !== managed);
  const switching = preference !== "auto" && managed !== "" && preference !== managed;

  const choose = async (value: string) => {
    if (value === preference) return;
    setLoading(true);
    try {
      const updated = await api.updateNodeAccelerator(node.uuid, value);
      onUpdated?.(updated);
      message.success(t("accelerator.updated"));
    } catch (e) {
      message.error(t("accelerator.failed", { detail: (e as Error).message }));
    } finally {
      setLoading(false);
    }
  };

  const options = [
    { value: "auto", label: t("accelerator.autoCurrent", { vendor: vendorName(managed) }) },
    ...vendors.map((v) => ({ value: v.vendor, label: describe(v) })),
  ];
  return (
    <div className="flex flex-col items-start gap-1">
      <Select size="sm" value={preference} disabled={loading} onValueChange={(value) => void choose(value)}>
        {/* A model name such as "AD102 [GeForce RTX 4090]" is longer than the
            select. The list sizes to its longest option, so it grows past the
            trigger, but no wider than the space the viewport leaves it: on a
            phone a long option ends in an ellipsis instead of running off the
            screen. */}
        <SelectTrigger aria-label={t("detail.accelerator")} aria-busy={loading || undefined} className="min-w-[220px]" />
        <SelectContent className="max-w-[var(--available-width)]">
          {options.map((option) => <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>)}
        </SelectContent>
      </Select>
      {switching ? (
        <span className="inline-flex items-start gap-1" role="status">
          <InfoCircleFilled className="mt-1 text-fg-brand" />
          <span>{t("accelerator.switching", { vendor: vendorName(preference) })}</span>
        </span>
      ) : (
        preference === "auto" &&
        unmanaged.length > 0 && (
          <span className="inline-flex items-start gap-1 text-fg-warning" role="alert">
            <ExclamationCircleFilled className="mt-1" />
            <span>{t("accelerator.unmanaged", { cards: unmanaged.map(describe).join(t("accelerator.separator")) })}</span>
          </span>
        )
      )}
    </div>
  );
}
