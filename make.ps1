<#
.SYNOPSIS
  Windows shim for the Makefile, for machines without GNU make.

.DESCRIPTION
  Mirrors the targets in ./Makefile. Everything here is a thin dispatcher -- the Makefile
  remains the source of truth, and this file exists so `.\make.ps1 demo` works on a clean
  Windows box with only Docker Desktop, Git and Python installed.

.EXAMPLE
  .\make.ps1 demo
  .\make.ps1 bench
  .\make.ps1 bench -Mode gateway
#>
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string] $Target = 'help',

    [ValidateSet('offline', 'gateway')]
    [string] $Mode = 'offline',

    [int] $Seed = 1337
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

$Py = if (Get-Command python -ErrorAction SilentlyContinue) { 'python' } else { 'py' }
$Results = 'benchmarks/results'

function Invoke-Step {
    param([string] $Description, [scriptblock] $Body)
    Write-Host ''
    Write-Host "==> $Description" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Description failed with exit code $LASTEXITCODE" }
}

function Invoke-Bash {
    param([string] $Script)
    $bash = (Get-Command bash -ErrorAction SilentlyContinue)
    if ($null -eq $bash) { throw 'bash not found. Install Git for Windows (it ships Git Bash).' }
    & $bash.Source $Script.Split(' ')
    if ($LASTEXITCODE -ne 0) { throw "bash $Script failed with exit code $LASTEXITCODE" }
}

switch ($Target) {

    'help' {
        Write-Host 'LLMRouter targets (Windows shim):' -ForegroundColor Green
        @(
            @{ n = 'demo';           d = 'Bring up the stack, seed traffic, print URLs' }
            @{ n = 'up';             d = 'Start the stack in the background' }
            @{ n = 'down';           d = 'Stop the stack and remove volumes' }
            @{ n = 'logs';           d = 'Tail gateway + guardrails logs' }
            @{ n = 'seed';           d = 'Regenerate every committed seed artifact' }
            @{ n = 'bench';          d = 'Run every benchmark (-Mode offline|gateway)' }
            @{ n = 'report';         d = 'Print the claim -> measured-number table' }
            @{ n = 'test';           d = 'Run Go and Python unit tests' }
            @{ n = 'lint';           d = 'Lint Go and Python' }
            @{ n = 'size-check';     d = 'Enforce the repo size budget' }
            @{ n = 'failover-demo';  d = 'Force a watchable mid-stream failover' }
        ) | ForEach-Object { '  {0,-16} {1}' -f $_.n, $_.d | Write-Host }
    }

    'demo'          { Invoke-Bash 'scripts/demo.sh' }
    'failover-demo' { Invoke-Bash 'scripts/failover_demo.sh' }
    'up'            { Invoke-Step 'starting the stack' { docker compose up -d --build } }
    'down'          { Invoke-Step 'stopping the stack' { docker compose down -v --remove-orphans } }
    'logs'          { docker compose logs -f gateway guardrails }
    'ps'            { docker compose ps }

    'seed' {
        Invoke-Step 'eval set'  { & $Py seed/generate_eval_set.py    --seed $Seed --out benchmarks/eval/dataset.jsonl }
        Invoke-Step 'cache pairs' { & $Py seed/generate_cache_pairs.py --seed $Seed --out benchmarks/cache/pairs.jsonl }
        Invoke-Step 'fixtures'  { & $Py seed/generate_fixtures.py     --seed $Seed --out seed/fixtures/responses.jsonl }
    }

    'bench' {
        Invoke-Step 'eval (cost + quality drift)' { & $Py benchmarks/eval/run_eval.py --mode $Mode --out "$Results/eval.json" }
        Invoke-Step 'cache calibration'           { & $Py benchmarks/cache/calibrate.py --out "$Results/cache_calibration.json" --plot docs/diagrams/cache_roc.png }
        Invoke-Step 'cache hit rate + latency'    { & $Py benchmarks/cache/run_cache_bench.py --mode $Mode --out "$Results/cache_bench.json" }
        Invoke-Step 'guardrail p99'               { & $Py benchmarks/guardrails/bench_guardrails.py --out "$Results/guardrails_latency.json" }
        Invoke-Step 'failover completion'         { & $Py benchmarks/failover/run_failover.py --mode $Mode --out "$Results/failover.json" }
        Invoke-Step 'cost attribution'            { & $Py benchmarks/attribution/run_attribution.py --mode $Mode --out "$Results/attribution.json" }
        Invoke-Step 'summary'                     { & $Py benchmarks/report.py }
    }

    'report'     { & $Py benchmarks/report.py }
    'test'       {
        Invoke-Step 'go tests (containerised)' {
            docker run --rm -v "${PWD}/services/gateway:/src" -w /src golang:1.23-alpine `
                sh -c 'apk add --no-cache git gcc musl-dev >/dev/null && go test -race ./...'
        }
        Invoke-Step 'python tests' { & $Py -m pytest -q services/guardrails/tests benchmarks/tests seed/tests }
    }
    'lint' {
        Invoke-Step 'ruff'  { & $Py -m ruff check services benchmarks seed }
        Invoke-Step 'mypy'  { & $Py -m mypy --strict services/guardrails/app }
    }
    'size-check' { Invoke-Bash 'scripts/size_check.sh' }
    'setup'      { Invoke-Step 'installing dev dependencies' { & $Py -m pip install -r requirements-dev.txt } }

    default { throw "Unknown target '$Target'. Run '.\make.ps1 help'." }
}
