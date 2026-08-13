param(
  [string]$K6Image = "grafana/k6:latest",
  [string]$BaseURL = "http://127.0.0.1:8080",
  [string]$OutputDir = "benchmarks/results"
)

$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$duration = $env:DURATION
if ([string]::IsNullOrWhiteSpace($duration)) { $duration = "60s" }

# Start the requested stack separately, then run this matrix. The script does
# not mutate proxy configuration so each result remains attributable to the
# stack that was intentionally started by the operator.
$cases = @(
  @{name="direct-1-backend"; url=$env:DIRECT_URL},
  @{name="proxy-1-backend"; url=$BaseURL},
  @{name="proxy-3-backend"; url=$BaseURL},
  @{name="proxy-rate-limit-enabled"; url=$BaseURL},
  @{name="proxy-rate-limit-disabled"; url=$BaseURL},
  @{name="proxy-failing-backend"; url=$BaseURL}
)
$concurrency = @(100, 1000, 10000)
foreach ($case in $cases) {
  if ([string]::IsNullOrWhiteSpace($case.url)) { continue }
  foreach ($vus in $concurrency) {
    $name = "$($case.name)-$vus"
    $summary = "/results/$name.json"
    docker run --rm --network host `
      -e "BASE_URL=$($case.url)" -e "VUS=$vus" -e "MAX_VUS=$vus" `
      -e "DURATION=$duration" -e "SCENARIO=$($case.name)" `
      -v "${PWD}/benchmarks:/scripts:ro" -v "${PWD}/$OutputDir:/results" $K6Image run `
      --summary-export=$summary /scripts/k6.js
  }
}
