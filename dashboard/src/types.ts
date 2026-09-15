export type Outcome = "match" | "different" | "error";

export interface Comparison {
  id: number;
  request_id: string;
  method: string;
  path: string;
  query: string;
  stable_status: number;
  candidate_status: number | null;
  stable_latency_ms: number;
  candidate_latency_ms: number;
  stable_body?: string;
  candidate_body?: string;
  status_match: boolean;
  body_match: boolean;
  candidate_slow: boolean;
  outcome: Outcome;
  differences: string[];
  candidate_error?: string;
  received_at: string;
}

export interface Stats {
  total: number;
  matched: number;
  different: number;
  errors: number;
  slow: number;
  avg_stable_latency_ms: number;
  avg_candidate_latency_ms: number;
}

export interface Deployment {
  stable_version: string;
  candidate_version: string;
  stable_url: string;
  candidate_url: string;
}
