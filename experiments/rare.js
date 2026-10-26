import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";

// Workload for the rare-fault experiment.
//
// The midterm workload hit every fault often enough that all six strategies
// found all five every time, so detection rate could not tell them apart and
// the research question went unanswered. Here each fault appears once in
// every 1000 requests, which over a 3000 request trial is about three
// chances to catch it.
//
// With P(detect) = 1 - (1 - rate)^3 that is roughly 27% at 10% sampling, 58%
// at 25%, 88% at 50% and 100% at full, so detection becomes probabilistic
// and the strategies separate.
//
// The faults sit at fixed positions inside each block of 1000 so every
// strategy sees them at exactly the same points in the run:
//
//   slot 100  R1  GET /api/products/12            price 49.99 -> 59.99
//   slot 300  R2  GET /api/products/8             200 -> 500
//   slot 500  R3  GET /api/products?category=audio  about 400ms slower
//   slot 700  R4  GET /api/users/7                exceeds the 2s timeout
//   slot 900  R5  POST /api/orders                total drops the shipping
//
// Everything else is clean traffic that must keep matching. None of the
// faulty endpoints appear in it.
const CLEAN = [
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
  "/api/users/1",
  "/api/users/2",
  "/api/users/3",
  "/api/products/99",
  "/health",
];

const BLOCK = 1000;
const FAULT_SLOTS = { 100: "R1", 300: "R2", 500: "R3", 700: "R4", 900: "R5" };

// Product 4 costs the same in both versions, so R5 shows only the shipping
// bug. See docs/regression-catalog.md.
const ORDER_BODY = JSON.stringify({
  customer: "sam@example.com",
  items: [{ product_id: 4, quantity: 1 }],
});

// /api/products/99 answers 404 on purpose and the order returns 201.
http.setResponseCallback(http.expectedStatuses(200, 201, 404));

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || "30s";

export const options = {
  scenarios: {
    rare: {
      executor: "constant-arrival-rate",
      rate: RATE,
      timeUnit: "1s",
      duration: DURATION,
      preAllocatedVUs: 50,
      maxVUs: 200,
    },
  },
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
  thresholds: {
    http_req_failed: ["rate<0.01"],
  },
};

export default function () {
  const i = exec.scenario.iterationInTest;
  const fault = FAULT_SLOTS[i % BLOCK];

  if (fault === "R5") {
    const res = http.post(GATEWAY + "/api/orders", ORDER_BODY, {
      headers: { "Content-Type": "application/json" },
    });
    check(res, {
      "order created by stable": (r) => r.status === 201,
      "client never sees the candidate total": (r) => !r.body.includes("179.99"),
    });
    return;
  }

  let path;
  switch (fault) {
    case "R1":
      path = "/api/products/12";
      break;
    case "R2":
      path = "/api/products/8";
      break;
    case "R3":
      path = "/api/products?category=audio";
      break;
    case "R4":
      path = "/api/users/7";
      break;
    default:
      path = CLEAN[i % CLEAN.length];
  }

  const res = http.get(GATEWAY + path);
  check(res, {
    "client got a stable response": (r) => r.status === 200 || r.status === 404,
    "candidate price never reaches the client": (r) => !r.body.includes("59.99"),
  });
}

export function handleSummary(data) {
  const name = __ENV.RUN_NAME || "run";
  // The overhead experiment reuses this workload but keeps its results
  // somewhere else.
  const dir = __ENV.RESULT_DIR || "rare-faults";
  return {
    [`/results/${dir}/${name}.k6.json`]: JSON.stringify(data, null, 2),
    stdout:
      `\n${name}: ${data.metrics.http_reqs.values.count} requests, ` +
      `p50 ${data.metrics.http_req_duration.values.med.toFixed(2)}ms, ` +
      `p95 ${data.metrics.http_req_duration.values["p(95)"].toFixed(2)}ms\n`,
  };
}
