import { vendorName } from "../vendorName";
import { ExclamationCircleFilled, InfoCircleFilled } from "@ant-design/icons";
import { Select, Space, Typography, message } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type AcceleratorPresence, type Node } from "../api";

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

  return (
    <Space direction="vertical" size={4}>
      <Select
        size="small"
        aria-label={t("detail.accelerator")}
        value={preference}
        loading={loading}
        disabled={loading}
        onChange={(value: string) => void choose(value)}
        style={{ minWidth: 220 }}
        // A model name such as "AD102 [GeForce RTX 4090]" is longer than the
        // select; the list grows to fit it instead of cutting it off.
        popupMatchSelectWidth={false}
        options={[
          { value: "auto", label: t("accelerator.autoCurrent", { vendor: vendorName(managed) }) },
          ...vendors.map((v) => ({ value: v.vendor, label: describe(v) })),
        ]}
      />
      {switching ? (
        <Space size={4} align="start" role="status">
          <InfoCircleFilled style={{ color: "#1677ff" }} />
          <Typography.Text>{t("accelerator.switching", { vendor: vendorName(preference) })}</Typography.Text>
        </Space>
      ) : (
        preference === "auto" &&
        unmanaged.length > 0 && (
          <Space size={4} align="start" role="alert">
            <ExclamationCircleFilled style={{ color: "#fa8c16" }} />
            <Typography.Text style={{ color: "#fa8c16" }}>
              {t("accelerator.unmanaged", { cards: unmanaged.map(describe).join(t("accelerator.separator")) })}
            </Typography.Text>
          </Space>
        )
      )}
    </Space>
  );
}
