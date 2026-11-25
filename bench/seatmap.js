import http from 'k6/http';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://api:8080';
const EVENT_ID = __ENV.EVENT_ID;

const seatmapDuration = new Trend('seatmap_duration', true);
const seatmapOutcomes = new Counter('seatmap_outcomes');

http.setResponseCallback(http.expectedStatuses(200));

export const options = {
  scenarios: {
    seatmap: {
      executor: 'constant-vus',
      vus: Number(__ENV.VUS || 50),
      duration: __ENV.DURATION || '30s',
    },
  },
  summaryTrendStats: ['avg', 'med', 'p(50)', 'p(90)', 'p(95)', 'p(99)', 'max'],
};

export function setup() {
  const res = http.get(`${BASE_URL}/events/${EVENT_ID}/seats`);
  if (res.status !== 200) {
    throw new Error(`seat map returned ${res.status}`);
  }
  return { seats: res.json('seats').length };
}

export default function () {
  const res = http.get(`${BASE_URL}/events/${EVENT_ID}/seats`);
  seatmapDuration.add(res.timings.duration);
  seatmapOutcomes.add(1, { status: String(res.status) });
}
