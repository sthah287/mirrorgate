import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";

// Traffic mix for the sampling experiment.
//
// The pattern is a fixed 20-slot cycle rather than random picks, so every
// sampling strategy is measured against exactly the same requests in exactly
// the same proportions. Over a 3000 request run each slot is used 150 times.
//
//   10 slots (50%)  normal product reads          -> should match
//    2 slots (10%)  /api/products/12              -> price regression
//    1 slot   (5%)  /api/products/8               -> candidate returns 500
//    1 slot   (5%)  /api/products?category=audio  -> candidate ~400ms slower
//    1 slot   (5%)  /api/users/7                  -> candidate exceeds the timeout
//    3 slots (15%)  user reads                    -> same data, reordered JSON
//    1 slot   (5%)  /api/products/99              -> 404 from both
//    1 slot   (5%)  /health                       -> should match
const PATTERN = [
  "/api/products/1",
  "/api/products/2",
  "/api/products/3",
  "/api/products/4",
  "/api/products/5",
  "/api/products/6",
  "/api/products/7",
  "/api/products/9",
  "/api/products/10",
  "/api/products/11",
  "/api/products/12",
  "/api/products/12",
  "/api/products/8",
  "/api/products?category=audio",
  "/api/users/7",
  "/api/users/1",
  "/api/users/2",
  "/api/users/3",
  "/api/products/99",
  "/health",
];

// /api/products/99 answers 404 on purpose, so 404 is a healthy response here.
// Without this k6 counts 5% of the run as failed requests.
http.setResponseCallback(http.expectedStatuses(200, 404));

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || "30s";

// A constant arrival rate keeps the offered load identical across strategies,
// so a latency difference between runs comes from the mirroring work and not
// from one run happening to push harder than another.
export const options = {
  scenarios: {
    mixed: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  // p(99) is not in k6's default summary and the experiment table wants it.
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
  // The client is served by the stable service, so anything slow here is the
  // gateway getting in the way.
  thresholds: {
    http_req_failed: ["rate<0.01"],
  },
};

export default function () {
  // iterationInTest is global across VUs, so the slot counts come out even.
  const path = PATTERN[exec.scenario.iterationInTest % PATTERN.length];
  const res = http.get(GATEWAY + path);

  // 404 is the expected answer for /api/products/99.
  check(res, {
    "client got a stable response": (r) => r.status === 200 || r.status === 404,
    "candidate price never reaches the client": (r) => !r.body.includes("59.99"),
  });
}

export function handleSummary(data) {
  const name = __ENV.RUN_NAME || "run";
  return {
    [`/results/progress-report-2/${name}.k6.json`]: JSON.stringify(data, null, 2),
    stdout: `\n${name}: ${data.metrics.http_reqs.values.count} requests, ` +
      `p50 ${data.metrics.http_req_duration.values.med.toFixed(2)}ms, ` +
      `p95 ${data.metrics.http_req_duration.values["p(95)"].toFixed(2)}ms\n`,
  };
}
