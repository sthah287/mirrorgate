import type { Comparison } from "../types";
import OutcomeBadge from "./OutcomeBadge";

interface Props {
  comparisons: Comparison[];
  selectedId: number | null;
  onSelect: (id: number) => void;
}

export default function ComparisonTable({ comparisons, selectedId, onSelect }: Props) {
  if (comparisons.length === 0) {
    return <p className="empty">No mirrored requests yet. Send some traffic to localhost:8080.</p>;
  }

  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Request</th>
            <th>Stable</th>
            <th>Candidate</th>
            <th>Result</th>
            <th>Details</th>
          </tr>
        </thead>
        <tbody>
          {comparisons.map((c) => (
            <tr
              key={c.id}
              className={c.id === selectedId ? "selected" : undefined}
              onClick={() => onSelect(c.id)}
            >
              <td className="muted nowrap">{new Date(c.received_at).toLocaleTimeString()}</td>
              <td className="mono">
                {c.method} {c.path}
                {c.query && <span className="muted">?{c.query}</span>}
              </td>
              <td className="mono nowrap">
                {c.stable_status} · {c.stable_latency_ms.toFixed(1)} ms
              </td>
              <td className="mono nowrap">
                {c.candidate_status ?? "—"} · {c.candidate_latency_ms.toFixed(1)} ms
              </td>
              <td className="nowrap">
                <OutcomeBadge outcome={c.outcome} />
                {c.candidate_slow && <span className="badge slow">SLOW</span>}
              </td>
              <td className="details">{c.candidate_error || c.differences[0] || ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
