import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import type { TranscriptState } from '@/stores/runs';
import RunEnvironmentLog from './RunEnvironmentLog.vue';

function mountLog(state: TranscriptState, text: string | null = null) {
  return mount(RunEnvironmentLog, {
    props: { state, text, error: 'HTTP 500' },
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
}

describe('RunEnvironmentLog', () => {
  it('asks for the log when shown, once', () => {
    expect(mountLog('idle').emitted('load')).toHaveLength(1);
    expect(mountLog('loaded', 'x').emitted('load')).toBeUndefined();
  });

  it('renders the log as wrapped monospace text, never HTML', () => {
    const text = `# environment.up\n<b>not html</b> ${'x'.repeat(500)}\n`;
    const pre = mountLog('loaded', text).get('[data-testid="environment-log"]');
    expect(pre.element.tagName).toBe('PRE');
    expect(pre.text()).toBe(text.trim());
    expect(pre.find('b').exists()).toBe(false);
    expect(pre.classes()).toEqual(
      expect.arrayContaining([
        'font-mono',
        'whitespace-pre-wrap',
        '[overflow-wrap:anywhere]',
      ]),
    );
  });

  it.each([
    ['missing', 'No environment log was uploaded for this run.'],
    ['error', "Couldn't load the environment log: HTTP 500"],
    ['loading', 'Loading…'],
  ] as const)('says when it is %s', (state, want) => {
    expect(mountLog(state).text()).toContain(want);
  });

  it('says when the log is empty', () => {
    expect(mountLog('loaded', '').text()).toContain(
      'The environment log is empty.',
    );
  });
});
