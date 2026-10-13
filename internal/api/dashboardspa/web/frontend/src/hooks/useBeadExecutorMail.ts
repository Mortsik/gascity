import { useEffect, useState } from 'react';
import { fetchExecutorMailForBead, type SupervisorMailItem } from '../supervisor/mailReads';

// Last-message-from-the-implementing-agent state for the bead detail surface
// (owner request 2026-10-13). Fetched only while the detail modal is open;
// failures render as an "unavailable" line, never a crash of the modal.

export interface BeadExecutorMailState {
  messages: SupervisorMailItem[];
  loading: boolean;
  error: string | null;
}

export function useBeadExecutorMail(active: boolean, beadId: string | null): BeadExecutorMailState {
  const [messages, setMessages] = useState<SupervisorMailItem[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!active || !beadId) {
      setMessages([]);
      setError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    void (async () => {
      try {
        const result = await fetchExecutorMailForBead(beadId);
        if (!cancelled) setMessages(result);
      } catch (err) {
        if (cancelled) return;
        setError(err instanceof Error ? err.message : 'fetch failed');
        setMessages([]);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [active, beadId]);

  return { messages, loading, error };
}
