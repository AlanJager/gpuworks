import { useRef, type PointerEvent as ReactPointerEvent, type ThHTMLAttributes } from "react";

export interface ResizeProps {
  /** Present only on a resizable header cell. */
  resizeWidth?: number;
  resizeMin?: number;
  resizeLabel?: string;
  /** The header's own name; without it the handle's label would join it. */
  headerLabel?: string;
  onResize?: (width: number) => void;
  onResizeEnd?: (width: number) => void;
}

/**
 * A table header cell with a drag handle on its right edge. Dragging follows
 * the pointer live and commits on release; a click on the handle never
 * reaches the header (no accidental sort).
 */
export default function ResizableHeaderCell({ resizeWidth, resizeMin = 48, resizeLabel, headerLabel, onResize, onResizeEnd, children, ...rest }: ThHTMLAttributes<HTMLTableCellElement> & ResizeProps) {
  const drag = useRef<{ startX: number; startWidth: number; width: number } | null>(null);
  if (!onResize || resizeWidth == null) return <th {...rest}>{children}</th>;
  const clamp = (value: number) => Math.max(resizeMin, Math.round(value));
  const down = (event: ReactPointerEvent<HTMLSpanElement>) => {
    event.preventDefault();
    event.stopPropagation();
    event.currentTarget.setPointerCapture?.(event.pointerId);
    drag.current = { startX: event.clientX, startWidth: resizeWidth, width: resizeWidth };
  };
  const move = (event: ReactPointerEvent<HTMLSpanElement>) => {
    if (!drag.current) return;
    const width = clamp(drag.current.startWidth + event.clientX - drag.current.startX);
    if (width === drag.current.width) return;
    drag.current.width = width;
    onResize(width);
  };
  const up = (event: ReactPointerEvent<HTMLSpanElement>) => {
    if (!drag.current) return;
    event.currentTarget.releasePointerCapture?.(event.pointerId);
    const { width } = drag.current;
    drag.current = null;
    onResizeEnd?.(width);
  };
  const key = (event: React.KeyboardEvent<HTMLSpanElement>) => {
    const step = event.key === "ArrowLeft" ? -16 : event.key === "ArrowRight" ? 16 : 0;
    if (!step) return;
    event.preventDefault();
    event.stopPropagation();
    const width = clamp(resizeWidth + step);
    onResize(width);
    onResizeEnd?.(width);
  };
  return (
    <th {...rest} aria-label={headerLabel}>
      {children}
      <span
        className="resource-table-resizer"
        role="separator"
        aria-orientation="vertical"
        aria-label={resizeLabel}
        aria-valuenow={resizeWidth}
        tabIndex={0}
        onPointerDown={down}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={up}
        onKeyDown={key}
        onClick={(event) => event.stopPropagation()}
      />
    </th>
  );
}
