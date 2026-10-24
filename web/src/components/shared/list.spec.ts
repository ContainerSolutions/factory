import { describe, expect, it } from 'vitest';
import { pickItem } from './list';

describe('pickItem', () => {
  const items = [{ id: 'a' }, { id: 'b' }];
  it.each<[unknown, string]>([
    ['b', 'b'],
    ['missing', 'a'],
    [undefined, 'a'],
    [['b'], 'a'],
  ])('picks %j as %s', (id, want) => {
    expect(pickItem(items, id)).toBe(want);
  });
  it('picks nothing from an empty list', () => {
    expect(pickItem([], 'a')).toBe('');
  });
});
