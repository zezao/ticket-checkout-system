import http from 'k6/http';
import { Counter, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://api:8080';
const EVENT_ID = __ENV.EVENT_ID;
const SCENARIO = __ENV.SCENARIO || 'uniform';

const holdDuration = new Trend('hold_duration', true);
const releaseDuration = new Trend('release_duration', true);
const holdOutcomes = new Counter('hold_outcomes');
const holdSuccess = new Counter('hold_success');
const holdConflicts = new Counter('hold_conflict');
const holdErrors = new Counter('hold_errors');

http.setResponseCallback(http.expectedStatuses(200, 201, 204, 409));

export const options = {
  scenarios: {
    holds: {
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
  const seats = res.json('seats');
  if (!seats || seats.length === 0) {
    throw new Error('benchmark event has no seats');
  }
  return { seats: seats.map((seat) => seat.id) };
}

export default function (data) {
  const userID = userFor(__VU, __ITER);
  const seatID = pickSeat(data.seats, SCENARIO);
  const headers = {
    'Content-Type': 'application/json',
    'X-User-ID': userID,
  };

  const res = http.post(
    `${BASE_URL}/events/${EVENT_ID}/holds`,
    JSON.stringify({ seat_id: seatID }),
    { headers },
  );

  holdDuration.add(res.timings.duration);
  holdOutcomes.add(1, { status: String(res.status) });

  if (res.status === 200 || res.status === 201) {
    holdSuccess.add(1);
    const holdID = res.json('hold.id');
    if (!holdID) {
      holdErrors.add(1);
      return;
    }

    const release = http.del(`${BASE_URL}/holds/${holdID}`, null, {
      headers: { 'X-User-ID': userID },
    });
    releaseDuration.add(release.timings.duration);
    if (release.status !== 204) {
      holdErrors.add(1);
    }
    return;
  }

  if (res.status === 409) {
    holdConflicts.add(1);
    return;
  }

  holdErrors.add(1);
}

function pickSeat(seats, scenario) {
  if (scenario === 'hot') {
    return seats[0];
  }

  if (scenario === 'skewed') {
    const hotCount = Math.max(1, Math.floor(seats.length * 0.1));
    if (Math.random() < 0.8) {
      return seats[Math.floor(Math.random() * hotCount)];
    }
    return seats[hotCount + Math.floor(Math.random() * (seats.length - hotCount))];
  }

  return seats[Math.floor(Math.random() * seats.length)];
}

function userFor(vu, iter) {
  const n = (vu * 1000003 + iter) % 0xffffffff;
  return `00000000-0000-4000-8000-${n.toString(16).padStart(12, '0')}`;
}
