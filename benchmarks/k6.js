import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  scenarios: {
    sustained: {
      executor: 'constant-arrival-rate',
      rate: Number(__ENV.RATE || 100),
      timeUnit: '1s',
      duration: __ENV.DURATION || '60s',
      preAllocatedVUs: Number(__ENV.VUS || 50),
      maxVUs: Number(__ENV.MAX_VUS || 500),
    },
  },
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<500', 'p(99)<1000'],
  },
};

export default function () {
  const response = http.get(`${__ENV.BASE_URL || 'http://127.0.0.1:8080'}/bench`, {
    headers: { 'X-API-Key': `k6-${__VU}` },
    tags: { scenario: __ENV.SCENARIO || 'default' },
  });
  check(response, { 'status is successful or limited': (r) => [200, 429, 502, 503].includes(r.status) });
  sleep(0.01);
}
