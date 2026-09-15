import { useCallback, useEffect, useState } from "react";
import { fetchComparisons, fetchStats } from "./api";
import type { Comparison, Deployment, Outcome, Stats } from "./types";
import SummaryCards from "./components/SummaryCards";
import ComparisonTable from "./components/ComparisonTable";
import ComparisonDetail from "./components/ComparisonDetail";

const REFRESH_MS = 3000;

const filters: { value: Outcome | "all"; label: string }[] = [
  { value: "all", label: "All" },
  { value: "different", label: "Different" },
  { value: "error", label: "Errors" },
  { value: "match", label: "Matched" },
];

export default function App() {
  const [stats, setStats] = useState<Stats | null>(null);
  const [deployment, setDeployment] = useState<Deployment | null>(null);
  const [comparisons, setComparisons] = useState<Comparison[]>([]);
  const [filter, setFilter] = useState<Outcome | "all">("all");
  const [selectedId, setSelectedId] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [updatedAt, setUpdatedAt] = useState<Date | null>(null);

  const load = useCallback(async () => {
    try {
      const [statsRes, list] = await Promise.all([fetchStats(), fetchComparisons(filter)]);
      setStats(statsRes.stats);
      setDeployment(statsRes.deployment);
      setComparisons(list);
      setUpdatedAt(new Date());
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load data");
    }
  }, [filter]);

  useEffect(() => {
    load();
    const timer = setInterval(load, REFRESH_MS);
    return () => clearInterval(timer);
  }, [load]);

  return (
    <div className="page">
      <header className="header">
        <div>
          <h1>MirrorGate</h1>
          {deployment && (
            <p className="deployment">
              Stable <strong>{deployment.stable_version}</strong>
              <span className="muted"> ({deployment.stable_url})</span>
              {"  →  "}
              Candidate <strong>{deployment.candidate_version}</strong>
              <span className="muted"> ({deployment.candidate_url})</span>
            </p>
          )}
        </div>
        <div className="refresh">
          {error ? (
            <span className="refresh-error">Gateway unreachable: {error}</span>
          ) : (
            updatedAt && <span className="muted">Updated {updatedAt.toLocaleTimeString()}</span>
          )}
        </div>
      </header>

      {stats && <SummaryCards stats={stats} />}

      <section className="panel">
        <div className="panel-header">
          <h2>Recent comparisons</h2>
          <div className="filters">
            {filters.map((f) => (
              <button
                key={f.value}
                className={f.value === filter ? "filter active" : "filter"}
                onClick={() => setFilter(f.value)}
              >
                {f.label}
              </button>
            ))}
          </div>
        </div>
        <ComparisonTable comparisons={comparisons} selectedId={selectedId} onSelect={setSelectedId} />
      </section>

      {selectedId !== null && (
        <ComparisonDetail id={selectedId} onClose={() => setSelectedId(null)} />
      )}
    </div>
  );
}
