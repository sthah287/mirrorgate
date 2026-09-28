import type { GatewayStats, SamplingStats } from "../types";

interface Props {
  sampling: SamplingStats;
  gateway: GatewayStats;
}

// How much of the incoming traffic is actually being mirrored. Without this
// the dashboard numbers look like a drop in traffic instead of a sampling
// setting that was turned down on purpose.
export default function SamplingPanel({ sampling, gateway }: Props) {
  const decisions = sampling.mirrored + sampling.skipped;
  const actual = decisions === 0 ? 0 : (sampling.mirrored / decisions) * 100;

  return (
    <section className="panel">
      <div className="panel-header">
        <h2>Sampling</h2>
        <span className="mono muted">
          {sampling.mode}
          {sampling.mode !== "full" && ` · target ${(sampling.rate * 100).toFixed(0)}%`}
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
