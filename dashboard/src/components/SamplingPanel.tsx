import type { EndpointRate, GatewayStats, SamplingStats } from "../types";

interface Props {
  sampling: SamplingStats;
  gateway: GatewayStats;
  endpoints: EndpointRate[];
}

// How much of the incoming traffic is actually being mirrored. Without this
// the dashboard numbers look like a drop in traffic instead of a sampling
// setting that was turned down on purpose.
export default function SamplingPanel({ sampling, gateway, endpoints }: Props) {
  const decisions = sampling.mirrored + sampling.skipped;
  const actual = decisions === 0 ? 0 : (sampling.mirrored / decisions) * 100;

  return (
    <section className="panel">
      <div className="panel-header">
        <h2>Sampling</h2>
        <span className="mono muted">
          {sampling.mode}
          {sampling.mode === "risk" && " · adaptive, 10% to 100% by risk score"}
          {sampling.mode !== "full" &&
            sampling.mode !== "risk" &&
            ` · target ${(sampling.rate * 100).toFixed(0)}%`}
        </span>
      </div>

      <div className="sampling-row">
        <Figure label="Eligible requests" value={decisions} />
        <Figure label="Mirrored" value={sampling.mirrored} />
        <Figure label="Skipped by sampling" value={sampling.skipped} />
        <Figure label="Actual rate" value={`${actual.toFixed(1)}%`} />
        <Figure label="Events published" value={gateway.published} />
        <Figure
          label="Publish failures"
          value={gateway.publish_failed}
          tone={gateway.publish_failed > 0 ? "bad" : undefined}
        />
      </div>

      {gateway.skipped_in_flight > 0 && (
        <p className="muted">
          {gateway.skipped_in_flight} request(s) were not mirrored because the shadow
          concurrency limit was full.
        </p>
      )}

      {endpoints.length > 0 && (
        <table className="rules">
          <thead>
            <tr>
              <th>Endpoint</th>
              <th>Requests</th>
              <th>Share</th>
              <th>Score</th>
              <th>Rate</th>
              <th>Why</th>
            </tr>
          </thead>
          <tbody>
            {endpoints.slice(0, 10).map((e) => (
              <tr key={e.endpoint}>
                <td className="mono">{e.endpoint}</td>
                <td className="mono">{e.requests}</td>
                <td className="mono">{(e.share * 100).toFixed(1)}%</td>
                <td className="mono">{e.score}</td>
                <td className="mono">{(e.rate * 100).toFixed(0)}%</td>
                <td className="muted">{e.reasons.join(", ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {endpoints.length > 10 && (
        <p className="muted">
          Showing the 10 highest rates of {endpoints.length} endpoints.
        </p>
      )}

      {sampling.rules && sampling.rules.length > 0 && (
        <table className="rules">
          <thead>
            <tr>
              <th>Pattern</th>
              <th>Rate</th>
            </tr>
          </thead>
          <tbody>
            {sampling.rules.map((r) => (
              <tr key={r.pattern}>
                <td className="mono">{r.pattern}</td>
                <td className="mono">{(r.rate * 100).toFixed(0)}%</td>
              </tr>
            ))}
            <tr>
              <td className="muted">everything else</td>
              <td className="mono">{(sampling.rate * 100).toFixed(0)}%</td>
            </tr>
          </tbody>
        </table>
      )}
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
