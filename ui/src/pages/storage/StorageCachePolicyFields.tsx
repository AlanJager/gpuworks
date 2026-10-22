import { InputNumber, Radio, Select, Space, Typography } from "antd";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { formatBytes } from "../../format";

/** Wire semantics are shared by creation, defaults and node policy editing. */
export default function StorageCachePolicyFields({ value, onChange, allowInherit = false, defaultBytes = 0, label, id }: {
  value: number | undefined;
  onChange: (value: number | undefined) => void;
  allowInherit?: boolean;
  defaultBytes?: number;
  label?: string;
  id?: string;
}) {
  const { t } = useTranslation("storage");
  const [unit, setUnit] = useState(1024 ** 3);
  const mode = value === undefined && allowInherit ? "inherit" : value === 0 || value === undefined ? "unlimited" : "custom";
  const name = label ?? t("cachePolicy.limit");
  return <Space direction="vertical">
    <div role="radiogroup" aria-label={name}><Radio.Group value={mode} onChange={(e) => onChange(e.target.value === "inherit" ? undefined : e.target.value === "unlimited" ? 0 : NaN)}>
      {allowInherit && <Radio value="inherit">{t("cachePolicy.inherit")}</Radio>}
      <Radio value="custom">{t("cachePolicy.custom")}</Radio>
      <Radio value="unlimited">{t("cachePolicy.unlimited")}</Radio>
    </Radio.Group></div>
    {mode === "custom" && <Space>
      <InputNumber id={id} aria-label={name} min={0} value={Number.isFinite(value) ? value! / unit : null} onChange={(next) => onChange(next == null || next <= 0 ? NaN : Math.round(next * unit))} />
      <Select aria-label={t("cachePolicy.unit")} value={unit} onChange={setUnit} options={[{ value: 1024 ** 3, label: "GiB" }, { value: 1024 ** 4, label: "TiB" }]} />
    </Space>}
    {mode === "inherit" && <Typography.Text type="secondary">{t("cachePolicy.effective", { value: defaultBytes === 0 ? t("cachePolicy.unlimited") : formatBytes(defaultBytes) })}</Typography.Text>}
  </Space>;
}
