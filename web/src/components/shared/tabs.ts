/** One tab of a TabList. */
export interface Tab {
  id: string;
  label: string;
  /** Shown after the label, e.g. how many objects the tab holds. */
  count?: number;
}

export function tabId(prefix: string, id: string): string {
  return `${prefix}-tab-${id}`;
}

export function panelId(prefix: string, id: string): string {
  return `${prefix}-panel-${id}`;
}

/** The attributes of the panel a tab controls. */
export function tabPanelAttrs(prefix: string, id: string) {
  return {
    id: panelId(prefix, id),
    role: 'tabpanel',
    'aria-labelledby': tabId(prefix, id),
    tabindex: 0,
  } as const;
}

/** The tab `wanted` names, or the first tab when it names none. */
export function pickTab(tabs: readonly Tab[], wanted: unknown): string {
  return tabs.find((t) => t.id === wanted)?.id ?? tabs[0]?.id ?? '';
}
