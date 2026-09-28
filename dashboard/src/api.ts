import type { Comparison, Deployment, GatewayStats, Outcome, SamplingStats, Stats } from "./types";

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) {
    throw new Error(`${path} returned ${res.status}`);
  }
  return res.json() as Promise<T>;
}

export function fetchStats() {
  return getJSON<{
    deployment: Deployment;
    stats: Stats;
    gateway: GatewayStats;
    sampling: SamplingStats;
  }>("/internal/stats");
}

export function fetchComparisons(outcome: Outcome | "all") {
  const params = new URLSearchParams({ limit: "100" });
  if (outcome !== "all") {
    params.set("outcome", outcome);
  }
  return getJSON<Comparison[]>(`/internal/comparisons?${params}`);
}

export function fetchComparison(id: number) {
  return getJSON<Comparison>(`/internal/comparisons/${id}`);
}
