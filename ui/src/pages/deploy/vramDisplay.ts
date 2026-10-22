import type { Breakdown } from "../../api";
import type { TFunction } from "i18next";

const GIB = 1024 ** 3;

const fallbackT = (key: string, params?: Record<string, unknown>) => {
  const required = String(params?.required ?? "");
  const capacity = String(params?.capacity ?? "");
  if (key === "vram.fitUnknown")
    return `This instance requires about ${required} GiB per GPU. Capacity is unknown.`;
  if (key === "vram.fits")
    return `Startup memory is about ${required} / ${capacity} GiB per GPU.`;
  return `Insufficient VRAM for startup: about ${required} GiB required, exceeding the ${capacity} GiB available limit.`;
};

export function formatVRAMFitSummary(
  breakdown: Breakdown,
  translate?: TFunction,
): { tone: "success" | "danger" | "secondary"; text: string } {
  const t = translate ?? fallbackT;
  const required = breakdown.per_gpu_bytes / GIB;
  const capacity = (breakdown.capacity_bytes || 0) / GIB;
  if (capacity <= 0) {
    return {
      tone: "secondary",
      text: t("vram.fitUnknown", { required: required.toFixed(1) }),
    };
  }
  if (breakdown.fits) {
    return {
      tone: "success",
      text: t("vram.fits", {
        required: required.toFixed(1),
        capacity: capacity.toFixed(1),
      }),
    };
  }
  return {
    tone: "danger",
    text: t("vram.insufficient", {
      required: required.toFixed(1),
      capacity: capacity.toFixed(1),
    }),
  };
}
