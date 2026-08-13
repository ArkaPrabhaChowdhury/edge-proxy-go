# Use Cases

## 1. Small API gateway for side projects

Put `edge-proxy` in front of a small Go, Node, or Python API when you want:

- request throttling without adding code to the app
- quick backend rotation across 2-3 app instances
- a dashboard for abuse visibility

## 2. Internal tool protection

Put `edge-proxy` in front of an internal admin API or automation endpoint when you want:

- per-IP or per-key control
- temporary blocking on repeated spikes
- local reproducibility for traffic-shaping experiments

## 3. Gateway experimentation

Use this repo as a starter when you want to prototype:

- custom rate-limit logic
- new identifier strategies
- richer event logging
- gateway policies in Go before adopting a larger proxy stack

## Local proof flow

1. Start the stack with `docker compose up --build`.
2. Send normal traffic to `http://localhost:8080/hello`.
3. Send burst traffic repeatedly.
4. Open `http://localhost:8081` and confirm rate-limit events show up.
5. Change config or code and repeat.
