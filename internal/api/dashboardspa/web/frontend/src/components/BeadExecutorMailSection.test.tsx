import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type { BeadExecutorMailState } from '../hooks/useBeadExecutorMail';
import { BeadExecutorMailSection } from './BeadExecutorMailSection';

function state(overrides: Partial<BeadExecutorMailState>): BeadExecutorMailState {
  return { messages: [], loading: false, error: null, ...overrides };
}

afterEach(cleanup);

describe('BeadExecutorMailSection', () => {
  it('renders nothing when there is no executor mail (calm absence)', () => {
    const { container } = render(<BeadExecutorMailSection state={state({})} />);
    expect(container.innerHTML).toBe('');
  });

  it('renders the newest message expanded with sender and body', () => {
    render(
      <BeadExecutorMailSection
        state={state({
          messages: [
            {
              id: 'm2',
              from: 'poe-pricer/agentforge-core.agentforge-executor-adhoc-47827dd7c3',
              subject: 'READY: execute al6lr4 — pass',
              body: 'landed 1234 on origin/master',
              created_at: '2026-10-13T16:08:55Z',
            },
            {
              id: 'm1',
              from: 'poe-pricer/agentforge-core.agentforge-executor-adhoc-47827dd7c3',
              subject: 'BLOCKED: waiting on lock',
              body: 'retrying',
              created_at: '2026-10-13T15:00:00Z',
            },
          ],
        })}
      />,
    );
    expect(screen.getByText('READY: execute al6lr4 — pass')).toBeTruthy();
    expect(screen.getByText(/landed 1234 on origin\/master/)).toBeTruthy();
    expect(screen.getAllByText(/agentforge-executor-adhoc-47827dd7c3/).length).toBeGreaterThan(0);
    // The older message lives inside the collapsed <details> toggle, never
    // expanded next to the latest one.
    const olderWrap = screen.getByText('BLOCKED: waiting on lock').closest('details');
    expect(olderWrap).not.toBeNull();
    expect(olderWrap?.open ?? false).toBe(false);
    expect(screen.getByText('1 earlier message')).toBeTruthy();
  });

  it('renders a calm unavailable line on fetch error', () => {
    render(<BeadExecutorMailSection state={state({ error: '503 upstream' })} />);
    expect(screen.getByText(/Executor mail unavailable/)).toBeTruthy();
  });
});
