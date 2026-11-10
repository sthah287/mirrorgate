import http from "k6/http";
import exec from "k6/execution";
import { check } from "k6";

// The rare-fault workload with the faults in random places.
//
// experiments/rare.js puts each fault at a fixed slot inside every block of
// 1000, which makes a run easy to reason about and makes one thing
// impossible to rule out: that a strategy did well because of where the
// faults happened to sit. Here the positions are drawn from a seed instead.
//
//   same seed  -> same positions, which is how every strategy in one trial
//                 sees exactly the same run
//   next trial -> a different seed, so the positions move
//
// The seed and the positions it produced are written next to the results, so
// a run can be repeated exactly and a detection can be tied to the
// occurrence that was caught rather than just to the fault.
//
//   R1  GET /api/products/12             price 49.99 -> 59.99
//   R2  GET /api/products/8              200 -> 500
//   R3  GET /api/products?category=audio about 400ms slower
//   R4  GET /api/users/7                 exceeds the 2s timeout
//   R5  POST /api/orders                 total drops the shipping
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

const FAULTS = ["R1", "R2", "R3", "R4", "R5"];

const GATEWAY = __ENV.GATEWAY_URL || "http://localhost:8080";
const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || "30s";
const SEED = Number(__ENV.SEED);
const TOTAL = Number(__ENV.TOTAL || 3000);
const PER_FAULT = Number(__ENV.PER_FAULT || 3);

if (!Number.isFinite(SEED)) {
  throw new Error("SEED is required: the point of this workload is that the positions are reproducible");
}

// mulberry32. Small, and the same sequence everywhere, which matters more
// here than the quality of the randomness: the positions only have to be
// unpredictable relative to the sampling strategies, not cryptographic.
function mulberry32(seed) {
  let a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Positions are drawn without replacement so no request carries two faults
// and every fault gets exactly PER_FAULT chances.
function plan() {
  const random = mulberry32(SEED);
  const taken = new Set();
  const positions = {};
  for (const fault of FAULTS) {
    positions[fault] = [];
    while (positions[fault].length < PER_FAULT) {
      const at = Math.floor(random() * TOTAL);
      if (taken.has(at)) {
        continue;
      }
      taken.add(at);
      positions[fault].push(at);
    }
    positions[fault].sort((a, b) => a - b);
  }
  return positions;
}

const POSITIONS = plan();
const AT = {};
for (const fault of FAULTS) {
  for (const at of POSITIONS[fault]) {
    AT[at] = fault;
  }
}

// Product 4 costs the same in both versions, so R5 shows only the shipping
// bug. See docs/regression-catalog.md.
const ORDER_BODY = JSON.stringify({
  customer: "sam@example.com",
  items: [{ product_id: 4, quantity: 1 }],
});

// /api/products/99 answers 404 on purpose and the order returns 201.
http.setResponseCallback(http.expectedStatuses(200, 201, 404));

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
  const fault = AT[i];

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
  const dir = __ENV.RESULT_DIR || "pr4/rare-random";
  // request_seq counts from 1, iterationInTest from 0, so the expected
  // sequence number of a fault is its position plus one. Both are written
  // out: the position is what the workload used, the sequence number is what
  // the comparison rows can be matched against.
  const seqs = {};
  for (const fault of FAULTS) {
    seqs[fault] = POSITIONS[fault].map((at) => at + 1);
  }
  return {
    [`/results/${dir}/${name}.k6.json`]: JSON.stringify(data, null, 2),
    [`/results/${dir}/${name}.faults.json`]: JSON.stringify(
      {
        seed: SEED,
        total: TOTAL,
        per_fault: PER_FAULT,
        positions: POSITIONS,
        expected_request_seq: seqs,
      },
      null,
      2,
    ),
    stdout:
      `\n${name}: ${data.metrics.http_reqs.values.count} requests, seed ${SEED}, ` +
      `p50 ${data.metrics.http_req_duration.values.med.toFixed(2)}ms, ` +
      `p95 ${data.metrics.http_req_duration.values["p(95)"].toFixed(2)}ms\n`,
  };
}
