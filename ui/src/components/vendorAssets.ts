// Horizontal accelerator vendor marks shipped under public/vendor-logos, so
// the UI stays self-contained and works offline. Sources (see NOTICE):
// - NVIDIA: NVIDIA's own lockup from NVIDIA/bionemo-framework
//   (docs/docs/assets/images/logo-white.svg, Apache-2.0), in the dark-on-light
//   colours of Wikimedia Commons "File:NVIDIA logo.svg".
// - AMD: Wikimedia Commons "File:AMD Logo.svg" (public domain text logo).
// - Ascend: the header logo of hiascend.com, Huawei's Ascend site.
// - T-Head: the header logo of t-head.cn, T-Head's site.
// - Hygon: its reviewed horizontal WebP.
// - Apple publishes no horizontal mark, so ours sets the Simple Icons glyph
//   (simple-icons 16.35.0, CC0-1.0) beside "Apple" outlined from Liberation
//   Sans Bold, in Apple's near-black, to sit with the other wordmarks.
export const VENDOR_ASSETS: Record<string, string> = {
  nvidia: "/vendor-logos/nvidia.svg",
  amd: "/vendor-logos/amd.svg",
  ascend: "/vendor-logos/ascend.svg",
  thead: "/vendor-logos/thead.png",
  hygon: "/vendor-logos/hygon.webp",
  apple: "/vendor-logos/apple.svg",
};
