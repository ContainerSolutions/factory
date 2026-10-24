import { describe, expect, it } from 'vitest';
import {
  MAX_ZOOM,
  MIN_ZOOM,
  clampZoom,
  fitWidth,
  panBy,
  wheelZoom,
  zoomAt,
} from './panZoom';

describe('panZoom', () => {
  it('zooms around a point: the content under it stays put', () => {
    const v = { x: 10, y: 20, k: 1 };
    const z = zoomAt(v, 2, 110, 70);
    // The content point under (110, 70) was (100, 50); it is still drawn there.
    expect(z.k).toBe(2);
    expect([z.x + 100 * z.k, z.y + 50 * z.k]).toEqual([110, 70]);
    // And back again.
    expect(zoomAt(z, 0.5, 110, 70)).toEqual(v);
  });

  it.each([
    [0.1, MIN_ZOOM],
    [0.25, 0.25],
    [1.5, 1.5],
    [10, MAX_ZOOM],
  ])('clamps zoom %d to %d', (k, want) => {
    expect(clampZoom(k)).toBe(want);
  });

  it('clamps at the limits without drifting the point', () => {
    const z = zoomAt({ x: 0, y: 0, k: 3 }, 2, 50, 50);
    expect(z).toEqual({ x: 0, y: 0, k: 3 });
  });

  it.each([
    [2000, 1000, 0.5],
    [500, 1000, 1],
    [20000, 1000, MIN_ZOOM],
    [0, 1000, 1],
    [2000, 0, 1],
  ])('fits %d wide content into %d, never above 1×: %d', (w, view, k) => {
    expect(fitWidth(w, view)).toEqual({ x: 0, y: 0, k });
  });

  it('pans, and zooms in on a negative wheel delta', () => {
    expect(panBy({ x: 1, y: 2, k: 2 }, 10, -5)).toEqual({ x: 11, y: -3, k: 2 });
    expect(wheelZoom(-100)).toBeGreaterThan(1);
    expect(wheelZoom(100)).toBeLessThan(1);
    expect(wheelZoom(0)).toBe(1);
  });
});
