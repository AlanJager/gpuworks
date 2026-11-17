import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Dialog, DialogClose, DialogContent, DialogHeader, DialogTitle, DialogFooter } from "../components/ui/dialog";
import { Button } from "../components/ui/button";
import { ConsoleIcon } from "./consoleIcons";
import { useOptionalAssistant } from "../assistant/AssistantContext";

/** Product dimensions and scroll ownership; Zeron owns portal, motion and focus. */
export function SideDrawer({ open, onClose, title, subtitle, inlineSubtitle = false, children, extra, footer, width = 720, className, busy = false, deferEscapeToModal = false }: {
  open: boolean; onClose: () => void; title: ReactNode; children?: ReactNode;
  /** A line under the title (identifiers, tags). */
  subtitle?: ReactNode;
  /** Resource names and secondary identifiers share a compact, wrapping title row. */
  inlineSubtitle?: boolean;
  /** Actions on the title row, right-aligned beside the close button, at their own size. */
  extra?: ReactNode; footer?: ReactNode; width?: number | string; className?: string; busy?: boolean; deferEscapeToModal?: boolean;
}) {
  const { t } = useTranslation("common");
  const assistantOpen = useOptionalAssistant()?.open ?? false;
  // A resident assistant and a detail are sibling workspaces. A modal focus
  // trap here would pull focus back from the assistant's independent portal.
  // Keep the detail one layer below it, so opening order cannot hide the
  // assistant on narrow screens; nested confirmations retain the normal layer.
  const zIndex = assistantOpen ? "calc(var(--layer-popover) - 1)" : undefined;
  return <Dialog open={open} modal={!assistantOpen} onOpenChange={(next, details) => {
    if (next) return;
    if (busy) { details.cancel(); return; }
    const target = details.reason === "focus-out" ? (details.event as FocusEvent).relatedTarget : details.event.target;
    if (assistantOpen && target instanceof Element && target.closest(".assistant-panel")) {
      details.cancel();
      details.allowPropagation();
      return;
    }
    if (deferEscapeToModal && details.reason === "escape-key" && Array.from(document.querySelectorAll<HTMLElement>(".ant-modal-wrap")).some(modal => modal.getClientRects().length > 0)) {
      details.cancel();
      details.allowPropagation();
      return;
    }
    onClose();
  }}>
    <DialogContent placement="right" className={`side-drawer rounded-none flex flex-col overflow-hidden ${className ?? ""}`}
      backdropClassName="side-drawer-backdrop" backdropStyle={{ zIndex }}
      style={{ width: typeof width === "number" ? `min(${width}px, calc(100vw - 32px))` : width, maxWidth: "var(--side-drawer-max-width, calc(100vw - 32px))", zIndex }}
      // The close button sits in the header row below. Zeron's own one is a
      // full-width sticky row laid over the header, which swallowed clicks on
      // the header actions next to the title.
      showCloseButton={false}>
      {/* Title and subtitle on the left, then the actions and the close button on
          the same row, as antd's Drawer laid them out. DialogHeader stacks its
          children and stretches them, which turned a switch into a full-width bar. */}
      <DialogHeader className="shrink-0">
        <div className="flex min-w-0 items-start gap-3">
          <div className={inlineSubtitle ? "flex min-w-0 flex-1 flex-wrap items-baseline gap-x-2 gap-y-1" : "min-w-0 flex-1"}>
            <DialogTitle className="min-w-0 break-words">{title}</DialogTitle>
            {subtitle != null && <div className={`${inlineSubtitle ? "" : "mt-1 "}min-w-0 break-words text-label text-fg-muted`}>{subtitle}</div>}
          </div>
          {extra != null && <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">{extra}</div>}
          <DialogClose aria-label={t("actions.close")} disabled={busy}
            render={<Button variant="ghost" iconOnly size="sm" className="shrink-0 -mr-2"><ConsoleIcon.close size={16} /></Button>} />
        </div>
      </DialogHeader>
      <div className="min-h-0 min-w-0 flex-1 overflow-auto detail-body">{children}</div>
      {footer && <DialogFooter>{footer}</DialogFooter>}
    </DialogContent>
  </Dialog>;
}
