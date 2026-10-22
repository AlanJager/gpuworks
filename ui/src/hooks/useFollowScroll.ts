import { useEffect, useRef, type RefObject } from "react";

/** How far from the bottom a user scroll has to land to stop following. */
export const FOLLOW_AWAY_THRESHOLD_PX = 48;

/**
 * A scroll event counts as the user's own only within this window after a
 * wheel, touch or key input. Programmatic scrolls and content growth carry no
 * input, so they can never switch following off.
 */
const USER_INPUT_WINDOW_MS = 250;

const SCROLL_UP_KEYS = new Set(["ArrowUp", "PageUp", "Home"]);

/**
 * useFollowScroll keeps a scroll container pinned to its bottom while
 * `follow` is on and reports when the user scrolls away from it.
 *
 * While following, every `contentVersion` change jumps straight to the bottom
 * (no smooth animation: its intermediate frames would read as the user
 * scrolling up). Turning `follow` back on jumps immediately. A non-empty text
 * selection inside the container pauses the jump so text being selected does
 * not slide out from under the pointer.
 *
 * `onUserScrollAway` fires when user input (wheel up, touch, ArrowUp/PageUp/
 * Home, or dragging the container's own scrollbar) leaves the view more than
 * FOLLOW_AWAY_THRESHOLD_PX above the bottom.
 */
export function useFollowScroll(
  ref: RefObject<HTMLElement>,
  follow: boolean,
  onUserScrollAway: () => void,
  contentVersion: unknown,
) {
  const onAwayRef = useRef(onUserScrollAway);
  onAwayRef.current = onUserScrollAway;
  const followRef = useRef(follow);
  followRef.current = follow;

  useEffect(() => {
    const element = ref.current;
    if (!element) return;
    let lastInputAt = Number.NEGATIVE_INFINITY;
    let draggingScrollbar = false;
    const markInput = () => { lastInputAt = performance.now(); };
    const onWheel = (event: WheelEvent) => { if (event.deltaY < 0) markInput(); };
    const onKeyDown = (event: KeyboardEvent) => { if (SCROLL_UP_KEYS.has(event.key)) markInput(); };
    // A press on the container itself, not on a message inside it, is a
    // press on its scrollbar; clicking or selecting text is not scrolling.
    const onPointerDown = (event: PointerEvent) => { if (event.target === element) draggingScrollbar = true; };
    const onPointerUp = () => { draggingScrollbar = false; };
    const onScroll = () => {
      if (!followRef.current) return;
      const userScrolled = draggingScrollbar || performance.now() - lastInputAt <= USER_INPUT_WINDOW_MS;
      if (!userScrolled) return;
      const distance = element.scrollHeight - element.scrollTop - element.clientHeight;
      if (distance > FOLLOW_AWAY_THRESHOLD_PX) onAwayRef.current();
    };
    element.addEventListener("wheel", onWheel, { passive: true });
    element.addEventListener("touchmove", markInput, { passive: true });
    element.addEventListener("keydown", onKeyDown);
    element.addEventListener("pointerdown", onPointerDown);
    element.addEventListener("scroll", onScroll, { passive: true });
    window.addEventListener("pointerup", onPointerUp);
    window.addEventListener("pointercancel", onPointerUp);
    return () => {
      element.removeEventListener("wheel", onWheel);
      element.removeEventListener("touchmove", markInput);
      element.removeEventListener("keydown", onKeyDown);
      element.removeEventListener("pointerdown", onPointerDown);
      element.removeEventListener("scroll", onScroll);
      window.removeEventListener("pointerup", onPointerUp);
      window.removeEventListener("pointercancel", onPointerUp);
    };
  }, [ref]);

  useEffect(() => {
    const element = ref.current;
    if (!follow || !element) return;
    const selection = document.getSelection?.();
    if (selection && !selection.isCollapsed && selection.anchorNode && element.contains(selection.anchorNode)) return;
    element.scrollTop = element.scrollHeight;
  }, [ref, follow, contentVersion]);
}
