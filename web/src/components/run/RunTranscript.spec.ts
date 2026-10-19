import { describe, expect, it } from 'vitest';
import { mount } from '@vue/test-utils';
import PrimeVue from 'primevue/config';
import { examples } from '@/mocks/examples';
import type { TranscriptState } from '@/stores/runs';
import RunTranscript from './RunTranscript.vue';
import { parseTranscript, type TranscriptEntry } from './transcript';

const entries = parseTranscript(examples.get('getTranscript') as string);

function mountTranscript(props: {
  available?: boolean;
  state: TranscriptState;
  entries?: TranscriptEntry[] | null;
  error?: string | null;
}) {
  return mount(RunTranscript, {
    props: { available: true, entries: null, ...props },
    global: { plugins: [[PrimeVue, { unstyled: true }]] },
  });
}

describe('RunTranscript', () => {
  it('loads only when asked', async () => {
    const wrapper = mountTranscript({ state: 'idle' });
    const button = wrapper.get('button');
    expect(button.text()).toBe('Load transcript');
    await button.trigger('click');
    expect(wrapper.emitted('load')).toEqual([[]]);
  });

  it('says when the run has no transcript', () => {
    const wrapper = mountTranscript({ available: false, state: 'idle' });
    expect(wrapper.find('button').exists()).toBe(false);
    expect(wrapper.text()).toContain('No transcript uploaded yet.');
  });

  it.each<[TranscriptState, string]>([
    ['missing', 'No transcript was uploaded for this run.'],
    ['error', "Couldn't load the transcript: HTTP 500"],
  ])('says so when %s', (state, text) => {
    const wrapper = mountTranscript({ state, error: 'HTTP 500' });
    expect(wrapper.text()).toContain(text);
  });

  it('shows the turns collapsed, and a turn on demand', async () => {
    const wrapper = mountTranscript({ state: 'loaded', entries });
    const turns = wrapper.findAll('[data-testid="transcript-turn"]');
    expect(turns.map((t) => t.get('button').text())).toEqual([
      '▸ Session 1 entry',
      '▸ Turn 1 1 entry',
      '▸ Turn 3 1 entry',
      '▸ Turn 17 1 entry',
    ]);
    expect(wrapper.findAll('[data-testid="transcript-entry"]')).toHaveLength(0);

    const toggle = turns[2]!.get('button');
    expect(toggle.attributes('aria-expanded')).toBe('false');
    await toggle.trigger('click');
    expect(toggle.attributes('aria-expanded')).toBe('true');
    const panel = turns[2]!.get(`#${toggle.attributes('aria-controls')}`);
    expect(panel.text()).toContain('tool');
    expect(panel.text()).toContain('read');
    expect(panel.text()).toContain('"path": "docs/oscal/import.md"');

    await toggle.trigger('click');
    expect(toggle.attributes('aria-expanded')).toBe('false');
    expect(wrapper.findAll('[data-testid="transcript-entry"]')).toHaveLength(0);
  });

  it('renders the text of a message as text', async () => {
    const wrapper = mountTranscript({
      state: 'loaded',
      entries: parseTranscript(
        '{"type":"message","role":"assistant","turn":1,"text":"<script>alert(1)</script>"}',
      ),
    });
    await wrapper
      .get('[data-testid="transcript-turn"] button')
      .trigger('click');
    expect(wrapper.find('script').exists()).toBe(false);
    expect(wrapper.get('[data-testid="transcript-entry"]').text()).toContain(
      'assistant',
    );
    expect(wrapper.get('[data-testid="transcript-entry"]').text()).toContain(
      '<script>alert(1)</script>',
    );
  });

  it('says when the transcript is empty', () => {
    const wrapper = mountTranscript({ state: 'loaded', entries: [] });
    expect(wrapper.text()).toContain('The transcript is empty.');
  });
});
