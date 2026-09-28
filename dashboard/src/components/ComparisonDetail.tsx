import { useEffect, useState } from "react";
import { fetchComparison } from "../api";
import type { Comparison } from "../types";
import OutcomeBadge from "./OutcomeBadge";

interface Props {
  id: number;
  onClose: () => void;
}

export default function ComparisonDetail({ id, onClose }: Props) {
  const [comparison, setComparison] = useState<Comparison | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    setComparison(null);
    setError(null);
    fetchComparison(id)
      .then((c) => {
        if (!cancelled) setComparison(c);
      })
      .catch((err: Error) => {
        if (!cancelled) setError(err.message);
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  return (
    <section className="panel">
      <div className="panel-header">
        <h2>Comparison #{id}</h2>
        <button className="filter" onClick={onClose}>
          Close
        </button>
      </div>

      {error && <p className="refresh-error">{error}</p>}
      {!comparison && !error && <p className="muted">Loading…</p>}

      {comparison && (
        <>
          <dl className="meta">
            <dt>Request</dt>
            <dd className="mono">
              {comparison.method} {comparison.path}
              {comparison.query && `?${comparison.query}`}
            </dd>
            <dt>Request ID</dt>
            <dd className="mono">{comparison.request_id}</dd>
            {comparison.trace_id && (
              <>
                <dt>Trace ID</dt>
                <dd className="mono">
                  <a
                    href={`http://localhost:16686/trace/${comparison.trace_id}`}
                    target="_blank"
                    rel="noreferrer"
                  >
                    {comparison.trace_id}
                  </a>
                </dd>
              </>
            )}
            <dt>Result</dt>
            <dd>
              <OutcomeBadge outcome={comparison.outcome} />
              {comparison.candidate_slow && <span className="badge slow">SLOW</span>}
            </dd>
            {comparison.candidate_error && (
              <>
                <dt>Candidate error</dt>
                <dd className="mono">{comparison.candidate_error}</dd>
              </>
            )}
          </dl>

          {comparison.differences.length > 0 && (
            <>
              <h3>Differences</h3>
              <ul className="diff-list">
                {comparison.differences.map((d) => (
                  <li key={d} className="mono">
                    {d}
                  </li>
                ))}
              </ul>
            </>
          )}

          <div className="bodies">
            <BodyView
              title={`Stable · ${comparison.stable_status} · ${comparison.stable_latency_ms.toFixed(1)} ms`}
              body={comparison.stable_body ?? ""}
            />
            <BodyView
              title={`Candidate · ${comparison.candidate_status ?? "no response"} · ${comparison.candidate_latency_ms.toFixed(1)} ms`}
              body={comparison.candidate_body ?? ""}
            />
          </div>
        </>
      )}
    </section>
  );
}

function BodyView({ title, body }: { title: string; body: string }) {
  return (
    <div className="body-view">
      <h3>{title}</h3>
      <pre>{prettyJSON(body) || <span className="muted">(empty)</span>}</pre>
    </div>
  );
}

function prettyJSON(body: string): string {
  try {
    return JSON.stringify(JSON.parse(body), null, 2);
  } catch {
    return body;
  }
}
