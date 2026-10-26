import type { OutboxStats } from "../types";

// The gateway writes comparison events to Postgres and the relay publishes
// them. Pending climbing, with the oldest row getting older, is what a broker
// outage looks like from here: the events are waiting, not gone.
export default function OutboxPanel({ outbox }: { outbox: OutboxStats }) {
  const backedUp = outbox.pending > 0;

  return (
    <section className="panel">
      <div className="panel-header">
        <h2>Outbox</h2>
        <span className="mono muted">
          {backedUp ? "relay is behind" : "drained"}
        </span>
      </div>

      <div className="sampling-row">
        <Figure label="Pending" value={outbox.pending} tone={backedUp ? "bad" : undefined} />
        <Figure label="Published" value={outbox.published} />
        <Figure
          label="Oldest pending"
          value={backedUp ? `${outbox.oldest_pending_sec.toFixed(1)} s` : "-"}
          tone={outbox.oldest_pending_sec > 30 ? "bad" : undefined}
        />
        <Figure label="Retried publishes" value={outbox.failed_attempts} />
      </div>
    </section>
  );
}

function Figure({ label, value, tone }: { label: string; value: number | string; tone?: "bad" }) {
  return (
    <div className="figure">
      <div className={`figure-value ${tone ?? ""}`}>{value}</div>
      <div className="card-label">{label}</div>
    </div>
  );
}
