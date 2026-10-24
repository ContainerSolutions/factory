/**
 * The work item graph's pan and zoom, as a pure transform: a content point (cx, cy) is drawn at
 * (x + cx·k, y + cy·k) in the viewport.
 */
export interface View {
  x: number;
  y: number;
  k: number;
}

export const MIN_ZOOM = 0.25;
export const MAX_ZOOM = 3;
/** The zoom factor of one button press or key press. */
export const ZOOM_STEP = 1.25;

export function clampZoom(k: number): number {
  return Math.min(MAX_ZOOM, Math.max(MIN_ZOOM, k));
}

/** Zoom by `factor` around the viewport point (px, py): the content under it stays put. */
export function zoomAt(v: View, factor: number, px: number, py: number): View {
  const k = clampZoom(v.k * factor);
  const f = k / v.k;
  return { x: px - (px - v.x) * f, y: py - (py - v.y) * f, k };
}

export function panBy(v: View, dx: number, dy: number): View {
  return { ...v, x: v.x + dx, y: v.y + dy };
}

/** The content's width fitted to the viewport's, never above 1× (a small graph isn't blown up). */
export function fitWidth(contentWidth: number, viewWidth: number): View {
  if (contentWidth <= 0 || viewWidth <= 0) return { x: 0, y: 0, k: 1 };
  return { x: 0, y: 0, k: clampZoom(Math.min(1, viewWidth / contentWidth)) };
}

/**
 * The wheel's zoom factor: ⌘/Ctrl+wheel and pinch (ctrl+wheel) zoom in on a negative delta. A
 * mouse wheel's notch (about 100) zooms about 1.2×; a pinch sends many small deltas.
 */
export function wheelZoom(deltaY: number): number {
  return Math.exp(-deltaY * 0.002);
}
