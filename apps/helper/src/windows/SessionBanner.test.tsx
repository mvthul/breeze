// @vitest-environment jsdom
import { render, screen, act } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { SessionBanner } from './SessionBanner';

describe('SessionBanner', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders the session label and initial elapsed time', () => {
    const startedAt = 1_000_000;
    vi.setSystemTime(startedAt);

    render(<SessionBanner label="Remote session active" startedAt={startedAt} />);

    expect(screen.getByText('Remote session active')).toBeInTheDocument();
    expect(screen.getByText('0:00')).toBeInTheDocument();
  });

  it('ticks elapsed time every second', () => {
    const startedAt = 1_000_000;
    vi.setSystemTime(startedAt);

    render(<SessionBanner label="Remote session active" startedAt={startedAt} />);

    expect(screen.getByText('0:00')).toBeInTheDocument();

    act(() => {
      vi.advanceTimersByTime(65_000); // 1 minute 5 seconds
    });

    expect(screen.getByText('1:05')).toBeInTheDocument();
  });
});
