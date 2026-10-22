import { Space, Tag, Tooltip } from "antd";
import { useTranslation } from "react-i18next";
import type { SystemLabel } from "../api";

// SystemLabelTags renders the labels the platform generates from a node's
// own facts. They are shown apart from the labels an operator writes because
// only the latter can be edited, and they read as "GPU model: RTX 4090"
// rather than as the stored pair: the key's namespace and the opaque value
// an id-valued label carries are what a selector matches on, not what a
// human needs to see, so both go in the tooltip.
//
// What to show comes from the server (display, display_code): a node or
// network name is shown as it is, and the few values that are platform
// vocabulary carry the code of an existing translation.
export function SystemLabelTags({
  labels,
  limit,
}: {
  labels?: SystemLabel[];
  limit?: number;
}) {
  const { t } = useTranslation("nodes");
  const shown = limit ? (labels ?? []).slice(0, limit) : (labels ?? []);
  const hidden = (labels ?? []).length - shown.length;
  if (shown.length === 0) return <>—</>;
  return (
    <Space size={[4, 4]} wrap>
      {shown.map((label) => (
        <Tooltip
          key={`${label.key}=${label.value}`}
          title={`${label.key}=${label.value}`}
        >
          <Tag>
            {systemLabelName(label.key, t)}: {systemLabelValue(label, t)}
          </Tag>
        </Tooltip>
      ))}
      {hidden > 0 && <Tag>{t("systemLabels.more", { count: hidden })}</Tag>}
    </Space>
  );
}

type Translate = (key: string, options?: Record<string, unknown>) => string;

// systemLabelName is the short name of a generated key. The reserved
// namespace is what tells a selector's author the label is the platform's,
// so it is not repeated in every tag.
export function systemLabelName(key: string, t: Translate): string {
  const short = key.split("/").pop() ?? key;
  return t(`systemLabels.key.${short}`, { defaultValue: short });
}

// systemLabelValue prefers the translation the server named and falls back
// to the text it sent, which is always populated.
export function systemLabelValue(label: SystemLabel, t: Translate): string {
  if (label.display_code) {
    return t(label.display_code, { defaultValue: label.display });
  }
  return label.display || label.value;
}
