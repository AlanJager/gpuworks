import { createI18n } from './helpers/dom.ts';
import React from 'react';
import assert from 'node:assert/strict';
import { afterEach, test } from 'node:test';
import { I18nextProvider } from 'react-i18next';
import { STREAM_INTERRUPTED_ERROR } from '../src/pages/playground/api';
import { ABORTED_ERROR } from '../src/pages/playground/conversations/streams';

const { render, screen, cleanup } = await import('@testing-library/react');
const { default: MessageBubble } = await import('../src/pages/playground/MessageBubble.tsx');
afterEach(cleanup);

for (const lng of ['en', 'zh'] as const) {
  test(`unexpected stream EOF uses localized error and retains partial text (${lng})`, async () => {
    const { i18n, locale } = await createI18n(['playground'], lng);
    render(<I18nextProvider i18n={i18n}><MessageBubble message={{ id: 'partial', role: 'assistant',
      content: 'partial answer', streaming: false, error: STREAM_INTERRUPTED_ERROR }} /></I18nextProvider>);
    assert.ok(screen.getByText('partial answer'));
    assert.equal(screen.getByRole('alert').textContent, locale.playground.pane.streamInterrupted);
    assert.equal(document.body.textContent?.includes(STREAM_INTERRUPTED_ERROR), false);
    assert.equal(document.body.textContent?.includes(locale.playground.pane.generationStopped), false);
    assert.equal(document.body.textContent?.includes(locale.playground.pane.interrupted), false);
  });
  test(`user abort keeps its distinct localized status (${lng})`, async () => {
    const { i18n, locale } = await createI18n(['playground'], lng);
    render(<I18nextProvider i18n={i18n}><MessageBubble message={{ id: 'stopped', role: 'assistant',
      content: 'partial answer', streaming: false, error: ABORTED_ERROR }} /></I18nextProvider>);
    assert.equal(screen.getByRole('alert').textContent, locale.playground.pane.generationStopped);
    assert.equal(document.body.textContent?.includes(locale.playground.pane.streamInterrupted), false);
  });
}
