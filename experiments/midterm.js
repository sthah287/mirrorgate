import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";

// Workload for the midterm experiment. It is the week 5 mix plus the write
// endpoint, and it is a fixed sequence rather than random picks, so every
// sampling strategy is measured against exactly the same requests in exactly
// the same order.
//
// One request in every 100 is the order POST (R5). The rest cycle this
// 20-slot pattern:
//
//   10 slots (50%)  ordinary product reads         -> should match
//    2 slots (10%)  /api/products/12               -> R1 price regression
//    1 slot   (5%)  /api/products/8                -> R2 candidate 500
//    1 slot   (5%)  /api/products?category=audio   -> R3 candidate ~400ms slower
//    1 slot   (5%)  /api/users/7                   -> R4 candidate exceeds the timeout
//    3 slots (15%)  user reads                     -> same data, reordered JSON
//    1 slot   (5%)  /api/products/99               -> 404 from both
//    1 slot   (5%)  /health                        -> should match
//
// In a 3000 request run that is 30 orders (1%), 297 hits on R1, and about
// 148 each on R2, R3, and R4. R5 being rare is the point: it is the case
// where a flat percentage has the most trouble.
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

const ORDER_EVERY = 100;

// Product 4 costs the same in both versions, so the only difference in the
// order response is the one R5 causes. See docs/regression-catalog.md.
const ORDER_BODY = JSON.stringify({
  customer: "sam@example.com",
  items: [{ product_id: 4, quantity: 1 }],
});

// /api/products/99 answers 404 on purpose and the order returns 201, so both
// are healthy here. Without this k6 counts them as failed requests.
http.setResponseCallback(http.expectedStatuses(200, 201, 404));

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || "30s";

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
  summaryTrendStats: ["avg", "min", "med", "p(95)", "p(99)", "max"],
  thresholds: {
    http_req_failed: ["rate<0.01"],
  },
};

export default function () {
  // iterationInTest is global across VUs, so the sequence is the same every
  // run no matter how the VUs interleave.
  const i = exec.scenario.iterationInTest;

  if (i % ORDER_EVERY === 0) {
    const res = http.post(GATEWAY + "/api/orders", ORDER_BODY, {
      headers: { "Content-Type": "application/json" },
    });
    check(res, {
      "order was created by stable": (r) => r.status === 201,
      // 184.99 is the stable total. The candidate's 179.99 must never
      // reach the client.
      "client never sees the candidate total": (r) => !r.body.includes("179.99"),
    });
    return;
  }

  // Keep the GET cycle continuous, so taking a slot out for the order
  // doesn't skew the proportions of the pattern.
  const postsBefore = i === 0 ? 0 : Math.floor((i - 1) / ORDER_EVERY) + 1;
  const path = PATTERN[(i - postsBefore) % PATTERN.length];

  const res = http.get(GATEWAY + path);
  check(res, {
    "client got a stable response": (r) => r.status === 200 || r.status === 404,
    "candidate price never reaches the client": (r) => !r.body.includes("59.99"),
  });
}

export function handleSummary(data) {
  const name = __ENV.RUN_NAME || "run";
  return {
    [`/results/midterm/${name}.k6.json`]: JSON.stringify(data, null, 2),
    stdout:
      `\n${name}: ${data.metrics.http_reqs.values.count} requests, ` +
      `p50 ${data.metrics.http_req_duration.values.med.toFixed(2)}ms, ` +
      `p95 ${data.metrics.http_req_duration.values["p(95)"].toFixed(2)}ms\n`,
  };
}
