import type { ReactNode } from "react";
import { Button, DatePicker, Popover, Select, Segmented, Space, Tooltip, Typography } from "antd";
import { DownloadOutlined, ExclamationCircleOutlined, ReloadOutlined } from "@ant-design/icons";
import dayjs, { type Dayjs } from "dayjs";
import { useTranslation } from "react-i18next";
import type { Granularity } from "../../analyticsApi";
import { QUICK_KEYS, type QuickKey, zonedParts, zonedToMs } from "./zoned";

export interface DeploymentOption {
  id: number;
  name: string;
}

interface Props {
  range: QuickKey | "custom";
  from: number;
  to: number;
  tz: string;
  granularity: Granularity;
  hourAllowed: boolean;
  /** How long the statistics are kept, shown on hovering the mark beside the picker. */
  retention: ReactNode;
  deployments: DeploymentOption[];
  deployment?: number;
  deploymentDisabled: boolean;
  exportDisabled: boolean;
  onQuick: (key: QuickKey) => void;
  onCustom: (from: number, to: number) => void;
  onGranularity: (g: Granularity) => void;
  onDeployment: (id?: number) => void;
  onExport: () => void;
  onRefresh: () => void;
}

// The picker works in the browser's wall clock; the statistics have their own
// zone. A picked time is the zone's clock reading it, and a time shown is the
// zone's clock put on the browser's.
function toPicker(ms: number, tz: string): Dayjs {
  const p = zonedParts(ms, tz);
  return dayjs(new Date(p.y, p.m - 1, p.d, p.h, p.mi));
}

function fromPicker(value: Dayjs, tz: string): number {
  return zonedToMs(value.year(), value.month() + 1, value.date(), value.hour(), value.minute(), tz);
}

// Toolbar is the filter strip every tab shares: quick ranges or a custom one,
// the granularity, the deployment, and the export of the table on screen.
export default function Toolbar(p: Props) {
  const { t } = useTranslation("analytics");
  return (
    <section aria-label={t("toolbar.ariaLabel")} className="analytics-toolbar" style={{ width: "100%" }}>
      <Space direction="vertical" size={12} style={{ width: "100%" }}>
        <Space wrap size={[16, 8]}>
          <Space wrap size={8} role="group" aria-label={t("toolbar.quickRanges")}>
            {QUICK_KEYS.map((key) => (
              <Button key={key} type={p.range === key ? "primary" : "default"} aria-pressed={p.range === key} onClick={() => p.onQuick(key)}>
                {t(`toolbar.ranges.${key}`)}
              </Button>
            ))}
          </Space>
          <Space size={8}>
            <Typography.Text type="secondary">{t("toolbar.custom")}</Typography.Text>
            <DatePicker.RangePicker
              showTime={{ format: "HH:mm" }}
              format="YYYY-MM-DD HH:mm"
              allowClear={false}
              value={[toPicker(p.from, p.tz), toPicker(p.to, p.tz)]}
              onChange={(value) => {
                if (!value?.[0] || !value[1]) return;
                const a = fromPicker(value[0], p.tz);
                const b = fromPicker(value[1], p.tz);
                p.onCustom(Math.min(a, b), Math.max(a, b));
              }}
              style={p.range === "custom" ? { borderColor: "#005bd4" } : undefined}
            />
            <Popover content={<div style={{ maxWidth: 320 }}>{p.retention}</div>}>
              <ExclamationCircleOutlined tabIndex={0} role="img" aria-label={t("toolbar.retention")} style={{ color: "#8c8c8c", cursor: "help" }} />
            </Popover>
          </Space>
        </Space>
        <Space wrap size={[16, 8]} style={{ width: "100%" }}>
          <Space size={8}>
            <Typography.Text type="secondary">{t("toolbar.granularity")}</Typography.Text>
            <Segmented
              value={p.granularity}
              onChange={(v) => p.onGranularity(v as Granularity)}
              options={[
                { value: "hour", label: t("toolbar.hour"), disabled: !p.hourAllowed },
                { value: "day", label: t("toolbar.day") },
              ]}
            />
          </Space>
          <Space size={8}>
            <Typography.Text type="secondary">{t("toolbar.deployment")}</Typography.Text>
            <Tooltip title={p.deploymentDisabled ? t("toolbar.deploymentNotForGpu") : undefined}>
              <Select
                allowClear
                showSearch
                optionFilterProp="label"
                style={{ minWidth: 200 }}
                aria-label={t("toolbar.deployment")}
                placeholder={t("toolbar.allDeployments")}
                disabled={p.deploymentDisabled}
                value={p.deployment}
                options={p.deployments.map((d) => ({ value: d.id, label: d.name }))}
                onChange={(v) => p.onDeployment(v ?? undefined)}
              />
            </Tooltip>
          </Space>
          <Space size={8} style={{ marginLeft: "auto" }}>
            <Button icon={<ReloadOutlined />} onClick={p.onRefresh} aria-label={t("toolbar.refresh")} />
            <Button type="primary" icon={<DownloadOutlined />} disabled={p.exportDisabled} onClick={p.onExport}>{t("toolbar.export")}</Button>
          </Space>
        </Space>
      </Space>
    </section>
  );
}
