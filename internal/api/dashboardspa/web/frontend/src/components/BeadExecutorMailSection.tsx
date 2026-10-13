import type { BeadExecutorMailState } from '../hooks/useBeadExecutorMail';
import type { SupervisorMailItem } from '../supervisor/mailReads';

// "Last executor message" section of the bead detail modal (owner request
// 2026-10-13): the closed-issues flow wants the implementing agent's final
// word — READY/BLOCKED mail carries the bead id and the executor session as
// sender, matched server-side by /mail/by-bead. The first (newest) message
// renders expanded; older ones collapse behind a toggle. Absence is calm:
// a closed issue with no executor mail is normal (manual close, sweep close).

export function BeadExecutorMailSection({ state }: { state: BeadExecutorMailState }) {
  if (state.loading) {
    return (
      <section aria-label="Executor messages">
        <h3 className="text-sm font-semibold tracking-wide uppercase text-fg-muted mb-2">
          Executor messages
        </h3>
        <p className="text-fg-muted italic text-sm">Fetching executor mail.</p>
      </section>
    );
  }
  if (state.error) {
    return (
      <section aria-label="Executor messages">
        <h3 className="text-sm font-semibold tracking-wide uppercase text-fg-muted mb-2">
          Executor messages
        </h3>
        <p className="text-fg-muted text-sm">Executor mail unavailable ({state.error}).</p>
      </section>
    );
  }
  if (state.messages.length === 0) {
    return null;
  }
  const [latest, ...older] = state.messages;
  if (latest === undefined) return null;
  return (
    <section aria-label="Executor messages">
      <h3 className="text-sm font-semibold tracking-wide uppercase text-fg-muted mb-2">
        Executor messages
      </h3>
      <ExecutorMailItem message={latest} highlight />
      {older.length > 0 && (
        <details className="mt-2">
          <summary className="text-fg-muted text-sm cursor-pointer">
            {older.length} earlier message{older.length === 1 ? '' : 's'}
          </summary>
          <div className="mt-2 space-y-2">
            {older.map((m) => (
              <ExecutorMailItem key={m.id} message={m} />
            ))}
          </div>
        </details>
      )}
    </section>
  );
}

function ExecutorMailItem({ message, highlight = false }: { message: SupervisorMailItem; highlight?: boolean }) {
  const stamp = message.created_at ? new Date(message.created_at).toLocaleString() : '';
  return (
    <div
      className={
        highlight
          ? 'rounded border border-border bg-bg-raised p-3'
          : 'rounded border border-border-dim p-3'
      }
    >
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1 mb-1">
        <span className="text-sm font-medium">{message.subject}</span>
        <span className="text-fg-faint text-xs">{stamp}</span>
      </div>
      <p className="text-fg-muted text-xs mb-2">
        from <code>{message.from}</code>
        {message.rig ? (
          <>
            {' · '}
            <code>{message.rig}</code>
          </>
        ) : null}
      </p>
      {message.body ? (
        <pre className="whitespace-pre-wrap break-words text-sm max-h-64 overflow-y-auto font-sans m-0">
          {message.body}
        </pre>
      ) : null}
    </div>
  );
}
