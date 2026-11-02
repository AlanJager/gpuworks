import { useEffect, useRef, useState } from "react";
import { Card, Statistic, Tooltip, Typography } from "antd";
import { InfoCircleOutlined } from "@ant-design/icons";
import { useTranslation } from "react-i18next";
import { bytes } from "./ImagePageHelpers";

type Props = {
  seriesCount: number;
  totalVersions: number;
  distributed: number;
  catalogBytes?: number;
  localBytes?: number;
  catalogUnknown: number;
  localUnknown: number;
};

type Summary = {
  key: string;
  value: string | number;
  unit?: string;
  help?: string;
  status?: string;
  unknownLabel?: string;
};

function SummaryHelp({ title, description }: { title: string; description: string }) {
  const [open, setOpen] = useState(false);
  const pinned = useRef(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const content = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    if (!open) return;
    const close = () => {
      pinned.current = false;
      setOpen(false);
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") close();
    };
    const onPointerDown = (event: PointerEvent) => {
      if (!trigger.current?.contains(event.target as Node) && !content.current?.contains(event.target as Node)) close();
    };
    document.addEventListener("keydown", onKeyDown);
    document.addEventListener("pointerdown", onPointerDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      document.removeEventListener("pointerdown", onPointerDown);
    };
  }, [open]);

  return (
    <Tooltip
      title={<span ref={content}>{description}</span>}
      trigger={["hover", "focus"]}
      open={open}
      onOpenChange={(next) => { if (!pinned.current) setOpen(next); }}
    >
      <button
        ref={trigger}
        type="button"
        className="image-summary-help"
        aria-label={title}
        aria-expanded={open}
        onClick={() => {
          pinned.current = !pinned.current;
          setOpen(pinned.current);
        }}
      >
        <InfoCircleOutlined />
      </button>
    </Tooltip>
  );
}

export default function ImageSummaryCards({
  seriesCount, totalVersions, distributed, catalogBytes, localBytes, catalogUnknown, localUnknown,
}: Props) {
  const { t } = useTranslation("images");
  const summaries: Summary[] = [
    { key: "series", value: seriesCount },
    { key: "images", value: totalVersions },
    { key: "distributed", value: distributed },
    ...[
      { key: "catalogSize", value: catalogBytes, total: totalVersions, unknown: catalogUnknown },
      { key: "localSize", value: localBytes, total: distributed, unknown: localUnknown },
    ].map(({ key, value, total, unknown }) => {
      const [amount, unit] = bytes(value).split(" ");
      return {
        key,
        value: amount,
        unit,
        help: t(`page.summary.${key}Help`),
        status: total === 0 ? undefined : value === undefined
          ? (unknown > 0 ? t("page.summary.noKnownSizes") : undefined)
          : t(unknown > 0 ? "page.summary.knownSubtotal" : "page.summary.allSizesKnown"),
        unknownLabel: unknown > 0 ? t(`page.summary.${key}Unknown`, { count: unknown }) : undefined,
      };
    }),
  ];

  return (
    <div className="image-summary-container">
      <div className="image-summary-grid">
        {summaries.map(({ key, value, unit, help, status, unknownLabel }) => {
          const title = t(`page.summary.${key}`);
          return (
            <Card key={key} className="image-summary-card" data-testid={`image-summary-${key}`}>
              <Statistic
                title={<div className="image-summary-title">
                  <span data-summary-text title={title}>{title}</span>
                  {help && <SummaryHelp title={title} description={help} />}
                </div>}
                value={value}
                valueRender={(node) => <span className="image-summary-value" data-testid="image-summary-value" data-summary-text>
                  {unit ? <>{value}<small> {unit}</small></> : node}
                </span>}
              />
              <div className="image-summary-info" data-testid="image-summary-info">
                {status && <Typography.Text type="secondary" data-summary-text title={status}>{status}</Typography.Text>}
                {unknownLabel && <Typography.Text type="warning" data-summary-text title={unknownLabel}>{unknownLabel}</Typography.Text>}
              </div>
            </Card>
          );
        })}
      </div>
    </div>
  );
}
