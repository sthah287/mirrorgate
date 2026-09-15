import type { Stats } from "../types";

export default function SummaryCards({ stats }: { stats: Stats }) {
  return (
    <div className="cards">
      <Card label="Requests mirrored" value={stats.total} />
      <Card label="Matched" value={stats.matched} tone="ok" />
      <Card label="Differences" value={stats.different} tone="warn" />
      <Card label="Candidate errors" value={stats.errors} tone="bad" />
      <Card label="Slow candidate" value={stats.slow} tone="warn" />
      <Card
        label="Avg latency (stable / candidate)"
        value={`${stats.avg_stable_latency_ms.toFixed(1)} / ${stats.avg_candidate_latency_ms.toFixed(1)} ms`}
      />
    </div>
  );
}

function Card({ label, value, tone }: { label: string; value: number | string; tone?: "ok" | "warn" | "bad" }) {
  return (
    <div className={`card ${tone ?? ""}`}>
      <div className="card-value">{value}</div>
      <div className="card-label">{label}</div>
    </div>
  );
}
