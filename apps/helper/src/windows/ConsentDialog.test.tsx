// @vitest-environment jsdom
import { fireEvent, render, screen, act } from '@testing-library/react';
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest';
import { ConsentDialog, type ConsentRequest } from './ConsentDialog';

const baseRequest: ConsentRequest = {
  sessionId: 'sess-123',
  technicianName: 'Alice Support',
  technicianEmail: 'alice@example.com',
  orgName: 'Acme Corp',
  timeoutMs: 30000,
  onTimeout: 'proceed',
};

describe('ConsentDialog', () => {
  it('renders technician name, email, and organization', () => {
    render(<ConsentDialog req={baseRequest} onDecision={vi.fn()} />);

    expect(screen.getByText('Alice Support')).toBeInTheDocument();
    expect(screen.getByText('alice@example.com')).toBeInTheDocument();
    expect(screen.getByText(/Acme Corp/)).toBeInTheDocument();
    expect(screen.getByText('AS')).toBeInTheDocument(); // Initials
  });

  it('falls back gracefully when technician identity fields are null', () => {
    const fallbackRequest: ConsentRequest = {
      sessionId: 'sess-456',
      technicianName: null,
      technicianEmail: null,
      orgName: null,
      timeoutMs: 10000,
      onTimeout: 'block',
    };

    render(<ConsentDialog req={fallbackRequest} onDecision={vi.fn()} />);

    expect(screen.getByText('A technician')).toBeInTheDocument();
    expect(screen.getByText('◐')).toBeInTheDocument();
    expect(screen.queryByText(/@/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Organization:/)).not.toBeInTheDocument();
  });

  it('focuses the Deny button by default (fail-safe)', () => {
    render(<ConsentDialog req={baseRequest} onDecision={vi.fn()} />);

    const denyButton = screen.getByRole('button', { name: 'Deny' });
    expect(document.activeElement).toBe(denyButton);
  });

  it('calls onDecision with allow when Allow button is clicked', () => {
    const onDecision = vi.fn();
    render(<ConsentDialog req={baseRequest} onDecision={onDecision} />);

    fireEvent.click(screen.getByRole('button', { name: 'Allow' }));
    expect(onDecision).toHaveBeenCalledWith(true, 'user');
  });

  it('calls onDecision with deny when Deny button is clicked', () => {
    const onDecision = vi.fn();
    render(<ConsentDialog req={baseRequest} onDecision={onDecision} />);

    fireEvent.click(screen.getByRole('button', { name: 'Deny' }));
    expect(onDecision).toHaveBeenCalledWith(false, 'user');
  });

  it('calls onDecision with deny when Escape key is pressed', () => {
    const onDecision = vi.fn();
    render(<ConsentDialog req={baseRequest} onDecision={onDecision} />);

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(onDecision).toHaveBeenCalledWith(false, 'user');
  });

  describe('countdown and timeout behavior', () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    afterEach(() => {
      vi.useRealTimers();
    });

    it('shows connecting label and triggers proceed on timeout when onTimeout is proceed', () => {
      const onDecision = vi.fn();
      let now = 1000;
      vi.spyOn(performance, 'now').mockImplementation(() => now);

      render(<ConsentDialog req={{ ...baseRequest, timeoutMs: 5000, onTimeout: 'proceed' }} onDecision={onDecision} />);

      expect(screen.getByText(/Connecting automatically in 0:05/)).toBeInTheDocument();

      act(() => {
        now += 5100;
        vi.advanceTimersByTime(5100);
      });

      expect(onDecision).toHaveBeenCalledWith(true, 'timeout');
    });

    it('shows declining label and triggers block on timeout when onTimeout is block', () => {
      const onDecision = vi.fn();
      let now = 1000;
      vi.spyOn(performance, 'now').mockImplementation(() => now);

      render(<ConsentDialog req={{ ...baseRequest, timeoutMs: 3000, onTimeout: 'block' }} onDecision={onDecision} />);

      expect(screen.getByText(/Declining automatically in 0:03/)).toBeInTheDocument();

      act(() => {
        now += 3100;
        vi.advanceTimersByTime(3100);
      });

      expect(onDecision).toHaveBeenCalledWith(false, 'timeout');
    });
  });
});
