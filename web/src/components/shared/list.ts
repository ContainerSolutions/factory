/** One entry of a list + detail layout: its id, its name and a short secondary line. */
export interface ListItem {
  id: string;
  label: string;
  secondary?: string;
}

/** The entry to show: the one `id` names when it is listed, else the first ('' for none). */
export function pickItem(
  items: readonly { id: string }[],
  id: unknown,
): string {
  return typeof id === 'string' && items.some((i) => i.id === id)
    ? id
    : (items[0]?.id ?? '');
}
