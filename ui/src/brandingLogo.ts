import { BRANDING_LOGO_MAX_BYTES } from "./branding";

/** LOGO_MAX_EDGE is the longest side a raster logo is scaled down to. */
export const LOGO_MAX_EDGE = 256;

/** Raster encodings tried in order until one fits the size limit. */
const ENCODINGS: { type: string; quality?: number }[] = [
  { type: "image/png" },
  { type: "image/webp", quality: 0.9 },
  { type: "image/webp", quality: 0.75 },
];

/** Edge lengths tried in order when no encoding fits at the larger size. */
const EDGES = [LOGO_MAX_EDGE, 128, 64];

/**
 * LogoCrop is the part of the image to keep, as fractions (0..1) of its
 * natural width and height.
 */
export interface LogoCrop {
  x: number;
  y: number;
  width: number;
  height: number;
}

/** FULL_CROP keeps the whole image. */
export const FULL_CROP: LogoCrop = { x: 0, y: 0, width: 1, height: 1 };

/** isFullCrop reports whether a crop keeps (practically) the whole image. */
export function isFullCrop(crop: LogoCrop): boolean {
  const eps = 0.001;
  return crop.x <= eps && crop.y <= eps && crop.width >= 1 - eps && crop.height >= 1 - eps;
}

/** squareCrop is the largest square centered in an image of this size. */
export function squareCrop(width: number, height: number): LogoCrop {
  if (width <= 0 || height <= 0) return FULL_CROP;
  const side = Math.min(width, height);
  return { x: (width - side) / 2 / width, y: (height - side) / 2 / height, width: side / width, height: side / height };
}

export class LogoError extends Error {
  constructor(readonly reason: "notImage" | "unreadable" | "tooLarge") {
    super(reason);
  }
}

export interface LogoCodec {
  /** dataURL reads a blob as a base64 data: URL. */
  dataURL(blob: Blob): Promise<string>;
  /**
   * rasterize draws the cropped part of the image scaled so its longest side
   * is at most maxEdge (never enlarged) and encodes it; it rejects when the
   * image cannot be decoded.
   */
  rasterize(file: Blob, crop: LogoCrop, maxEdge: number, type: string, quality?: number): Promise<Blob>;
}

/**
 * prepareLogo turns the cropped part of an uploaded image into a logo data:
 * URL within the server's limit. An uncropped SVG that already fits is kept
 * as is (it stays sharp at any size); everything else is cut to the crop,
 * scaled down and re-encoded, taking the first
 * result that fits: PNG keeps transparency losslessly, WebP is the fallback,
 * and a smaller edge is the last resort.
 */
export async function prepareLogo(file: File, crop: LogoCrop = FULL_CROP, codec: LogoCodec = browserCodec): Promise<string> {
  if (!file.type.startsWith("image/")) throw new LogoError("notImage");
  if (file.type === "image/svg+xml" && file.size <= BRANDING_LOGO_MAX_BYTES && isFullCrop(crop)) {
    return codec.dataURL(new Blob([file], { type: "image/svg+xml" }));
  }
  for (const edge of EDGES) {
    for (const { type, quality } of ENCODINGS) {
      let blob: Blob;
      try {
        blob = await codec.rasterize(file, crop, edge, type, quality);
      } catch {
        throw new LogoError("unreadable");
      }
      if (blob.size > 0 && blob.size <= BRANDING_LOGO_MAX_BYTES) return codec.dataURL(blob);
    }
  }
  throw new LogoError("tooLarge");
}

const browserCodec: LogoCodec = {
  dataURL: (blob) => new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.onerror = () => reject(reader.error ?? new Error("read failed"));
    reader.readAsDataURL(blob);
  }),
  rasterize: async (file, crop, maxEdge, type, quality) => {
    const url = URL.createObjectURL(file);
    try {
      const image = new Image();
      image.src = url;
      await image.decode();
      const naturalWidth = image.naturalWidth || maxEdge;
      const naturalHeight = image.naturalHeight || maxEdge;
      const sx = crop.x * naturalWidth;
      const sy = crop.y * naturalHeight;
      const width = Math.max(1, crop.width * naturalWidth);
      const height = Math.max(1, crop.height * naturalHeight);
      const scale = Math.min(1, maxEdge / Math.max(width, height));
      const canvas = document.createElement("canvas");
      canvas.width = Math.max(1, Math.round(width * scale));
      canvas.height = Math.max(1, Math.round(height * scale));
      const context = canvas.getContext("2d");
      if (!context) throw new Error("no 2d context");
      context.imageSmoothingQuality = "high";
      context.drawImage(image, sx, sy, width, height, 0, 0, canvas.width, canvas.height);
      return await new Promise<Blob>((resolve, reject) => {
        canvas.toBlob((blob) => blob ? resolve(blob) : reject(new Error("encode failed")), type, quality);
      });
    } finally {
      URL.revokeObjectURL(url);
    }
  },
};
