// copyText works in secure contexts through the modern Clipboard API and
// falls back to the synchronous selection API for plain-HTTP deployments.
// Callers should expose the text for manual selection when both paths fail.
export async function copyText(value: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(value);
      return true;
    }
  } catch {
    // Permission-denied and insecure-context failures continue to the HTTP
    // fallback below.
  }

  if (typeof document === "undefined" || typeof document.execCommand !== "function") return false;
  // The selection fallback has to move focus to its textarea; hand it back
  // afterwards so a keyboard user stays on the button they pressed.
  const previousFocus = document.activeElement;
  const textarea = document.createElement("textarea");
  textarea.value = value;
  textarea.readOnly = true;
  textarea.style.position = "fixed";
  textarea.style.opacity = "0";
  textarea.style.pointerEvents = "none";
  document.body.appendChild(textarea);
  textarea.focus();
  textarea.select();
  textarea.setSelectionRange(0, textarea.value.length);
  try {
    return document.execCommand("copy");
  } catch {
    return false;
  } finally {
    textarea.remove();
    if (previousFocus instanceof HTMLElement) previousFocus.focus({ preventScroll: true });
  }
}
