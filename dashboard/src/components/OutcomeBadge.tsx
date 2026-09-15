import type { Outcome } from "../types";

export default function OutcomeBadge({ outcome }: { outcome: Outcome }) {
  return <span className={`badge ${outcome}`}>{outcome.toUpperCase()}</span>;
}
