param([switch]$NoFrontend, [switch]$NoWorker, [switch]$StopOnExit)
$ErrorActionPreference = 'Stop'
Push-Location $PSScriptRoot
$services = @('mysql', 'redis', 'rabbitmq', 'api')
if (-not $NoWorker -and $env:START_WORKER -ne '0') { $services += 'worker' }
if (-not $NoFrontend -and $env:START_FRONTEND -ne '0') { $services += 'web' }
try {
    & docker compose up -d --build @services
    if ($LASTEXITCODE -ne 0) { throw 'Docker Compose startup failed' }
    & docker compose logs -f @services
} finally {
    if ($StopOnExit -or $env:STOP_DOCKER -eq '1') { & docker compose stop @services }
    Pop-Location
}
