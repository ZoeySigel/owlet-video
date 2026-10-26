[CmdletBinding()]
param(
    [ValidateSet('smoke', 'full')]
    [string]$Profile = 'full',
    [string]$Output = '',
    [string]$Duration = '60s',
    [string]$Warmup = '30s',
    [ValidateRange(1, 1000)]
    [int]$Rounds = 3,
    [ValidatePattern('^[1-9][0-9]*(,[1-9][0-9]*)*$')]
    [string]$Rates = '20,100',
    [ValidateRange(4, 10000000)]
    [int]$Users = 1000,
    [ValidateRange(100, 10000000)]
    [int]$Videos = 10000,
    [long]$Seed = 20261024
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$composeFile = Join-Path $repoRoot 'compose.cache-validation.yml'
$projectName = 'owlet-cache-validation'
$runID = [DateTimeOffset]::Now.ToString('yyyyMMdd-HHmmss-fff')
$toolRoot = Join-Path $repoRoot '.tools/cache-validation'
$logRoot = Join-Path $toolRoot ('logs/' + $runID)
$outputPath = $null
$dockerExe = $null
$composeTouched = $false
$runExitCode = 1
$toolExitCode = $null
$lockHeld = $false
$runLock = $null
$startedAt = [DateTimeOffset]::Now

function Find-Executable {
    param([string]$Name, [string[]]$Candidates)

    $found = Get-Command $Name -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($null -ne $found) { return $found.Source }
    foreach ($candidate in $Candidates) {
        if (-not [string]::IsNullOrWhiteSpace($candidate) -and
            (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            return [IO.Path]::GetFullPath($candidate)
        }
    }
    throw "Cannot locate $Name. Install it or add its executable directory to PATH."
}

function Invoke-LoggedCommand {
    param([string]$Executable, [string[]]$Arguments, [string]$LogName)

    $lines = [Collections.Generic.List[string]]::new()
    $logPath = Join-Path $logRoot $LogName
    $savedPreference = $ErrorActionPreference
    # Windows PowerShell represents native stderr as ErrorRecord objects. Check
    # the exit code ourselves so harmless diagnostics do not abort the pipeline.
    $ErrorActionPreference = 'Continue'
    try {
        & $Executable @Arguments 2>&1 | ForEach-Object {
            $line = $_.ToString()
            $lines.Add($line)
            Add-Content -LiteralPath $logPath -Value $line -Encoding UTF8
            Write-Host $line
        }
        $commandExitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $savedPreference
    }
    return [pscustomobject]@{ ExitCode = $commandExitCode; Lines = $lines.ToArray() }
}

function Assert-CommandSuccess {
    param($Result, [string]$Message)
    if ($Result.ExitCode -ne 0) {
        throw "$Message (exit code $($Result.ExitCode)). Logs: $logRoot"
    }
}

function Read-PublishedPort {
    param([string]$Service, [int]$ContainerPort)

    $result = Invoke-LoggedCommand $dockerExe `
        ($composeArgs + @('port', $Service, $ContainerPort.ToString())) 'ports.log'
    Assert-CommandSuccess $result "Could not read the published port for $Service"
    $bindings = @($result.Lines | Where-Object { $_ -match '^127\.0\.0\.1:([0-9]+)$' })
    if ($bindings.Count -ne 1 -or $bindings[0] -notmatch '^127\.0\.0\.1:([0-9]+)$') {
        throw "Expected one localhost-only binding for $Service; got: $($result.Lines -join ', ')"
    }
    $port = [int]$Matches[1]
    if ($port -lt 1 -or $port -gt 65535) { throw "Invalid port for ${Service}: $port" }
    return $port
}

try {
    if ([string]::IsNullOrWhiteSpace($Output)) {
        $outputPath = Join-Path $repoRoot ('docs/cache-validation/results/' + $runID)
    }
    elseif ([IO.Path]::IsPathRooted($Output)) {
        $outputPath = [IO.Path]::GetFullPath($Output)
    }
    else {
        # Relative output paths are always relative to the repository, not the
        # caller's current shell directory.
        $outputPath = [IO.Path]::GetFullPath((Join-Path $repoRoot $Output))
    }
    if (Test-Path -LiteralPath $outputPath) {
        if (-not (Test-Path -LiteralPath $outputPath -PathType Container) -or
            @(Get-ChildItem -LiteralPath $outputPath -Force).Count -gt 0) {
            throw "Output must be a new or empty directory; existing results will not be overwritten: $outputPath"
        }
    }

    $runLock = [Threading.Mutex]::new($false, 'OwletCacheValidation')
    try { $lockHeld = $runLock.WaitOne(0) }
    catch [Threading.AbandonedMutexException] { $lockHeld = $true }
    if (-not $lockHeld) {
        throw 'Another cache validation run is using the dedicated Compose project. Wait for it to finish.'
    }

    New-Item -ItemType Directory -Path $logRoot -Force | Out-Null
    $programFilesPath = [Environment]::GetFolderPath('ProgramFiles')
    $dockerExe = Find-Executable 'docker' @(
        (Join-Path $programFilesPath 'Docker/Docker/resources/bin/docker.exe'),
        (Join-Path $repoRoot '.tools/bin/docker.exe')
    )
    $goExe = Find-Executable 'go' @(
        (Join-Path $programFilesPath 'Go/bin/go.exe'),
        (Join-Path $repoRoot '.tools/bin/go.exe')
    )
    $composeArgs = @('compose', '--project-name', $projectName, '--project-directory', $repoRoot, '--file', $composeFile)

    $result = Invoke-LoggedCommand $dockerExe @('info', '--format', '{{.ServerVersion}}') 'docker-info.log'
    if ($result.ExitCode -ne 0) {
        throw 'Docker Engine is not running or is unreachable. Start Docker Desktop with Linux containers, wait until it is ready, then rerun this script.'
    }
    Assert-CommandSuccess (Invoke-LoggedCommand $dockerExe @('compose', 'version') 'compose-version.log') 'Docker Compose v2 is required'
    Assert-CommandSuccess (Invoke-LoggedCommand $goExe @('version') 'go-version.log') 'Go is unavailable'
    Assert-CommandSuccess (Invoke-LoggedCommand $dockerExe ($composeArgs + @('config')) 'compose-config.log') 'The isolated Compose configuration is invalid'

    $toolPath = Join-Path $toolRoot 'cachevalidation.exe'
    Push-Location (Join-Path $repoRoot 'backend')
    try {
        Assert-CommandSuccess (Invoke-LoggedCommand $goExe @('build', '-o', $toolPath, './tools/cachevalidation') 'build.log') 'Could not build the cache validation tool'
    }
    finally { Pop-Location }

    $composeTouched = $true
    Assert-CommandSuccess (Invoke-LoggedCommand $dockerExe ($composeArgs + @('up', '--detach', '--wait', '--wait-timeout', '240')) 'compose-up.log') 'The isolated dependencies did not become healthy'
    $mysqlPort = Read-PublishedPort 'mysql' 3306
    $redisPort = Read-PublishedPort 'redis' 6379
    $dsn = 'cache_validation:cache-validation-local-password@tcp(127.0.0.1:' +
        $mysqlPort + ')/cache_validation?charset=utf8mb4&parseTime=true&loc=UTC'
    $toolArgs = @(
        '-profile', $Profile,
        '-mysql-dsn', $dsn,
        '-redis-addr', ('127.0.0.1:' + $redisPort),
        '-output', $outputPath
    )
    $optionalFlags = @{
        Duration = '-duration'; Warmup = '-warmup'; Rounds = '-rounds';
        Rates = '-rates'; Users = '-users'; Videos = '-videos'; Seed = '-seed'
    }
    foreach ($parameter in @('Duration', 'Warmup', 'Rounds', 'Rates', 'Users', 'Videos', 'Seed')) {
        if ($PSBoundParameters.ContainsKey($parameter)) {
            $value = (Get-Variable -Name $parameter -ValueOnly).ToString()
            $toolArgs += @($optionalFlags[$parameter], $value)
        }
    }
    Write-Host "Running $Profile validation. Results: $outputPath"
    $result = Invoke-LoggedCommand $toolPath $toolArgs 'validation.log'
    $toolExitCode = $result.ExitCode
    $runExitCode = $toolExitCode
}
catch {
    Write-Error $_ -ErrorAction Continue
    if (Test-Path -LiteralPath $logRoot -PathType Container) {
        Add-Content -LiteralPath (Join-Path $logRoot 'runner-error.log') -Value $_.ToString() -Encoding UTF8
    }
}
finally {
    if ($composeTouched -and $null -ne $dockerExe) {
        try {
            $null = Invoke-LoggedCommand $dockerExe ($composeArgs + @('ps', '--all')) 'compose-ps.log'
            $null = Invoke-LoggedCommand $dockerExe ($composeArgs + @('logs', '--no-color', '--timestamps')) 'compose-services.log'
            $downResult = Invoke-LoggedCommand $dockerExe ($composeArgs + @('down', '--timeout', '15')) 'compose-down.log'
            if ($downResult.ExitCode -ne 0) {
                Write-Warning "Could not stop the dedicated project. Inspect $logRoot/compose-down.log."
                if ($runExitCode -eq 0) { $runExitCode = 1 }
            }
        }
        catch {
            Write-Warning "Dependency cleanup failed: $_"
            if ($runExitCode -eq 0) { $runExitCode = 1 }
        }
    }
    if ($lockHeld -and $null -ne $outputPath -and
        (Test-Path -LiteralPath $logRoot -PathType Container)) {
        try {
            $runner = [ordered]@{
                project = $projectName; profile = $Profile; output = $outputPath;
                started_at = $startedAt.ToString('o');
                finished_at = [DateTimeOffset]::Now.ToString('o');
                exit_code = $runExitCode; tool_exit_code = $toolExitCode;
                powershell = $PSVersionTable.PSVersion.ToString();
                parameters = $PSBoundParameters
            }
            $runner | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $logRoot 'runner.json') -Encoding UTF8
            New-Item -ItemType Directory -Path $outputPath -Force | Out-Null
            $destination = Join-Path $outputPath 'runner-logs'
            if (Test-Path -LiteralPath $destination) {
                Write-Warning "Runner logs already exist; preserved new logs at $logRoot."
            }
            else { Copy-Item -LiteralPath $logRoot -Destination $destination -Recurse }
            $runExitCode.ToString() | Set-Content -LiteralPath (Join-Path $outputPath 'exit-code.txt') -Encoding ASCII
            if ($null -ne $toolExitCode) {
                $toolExitCode.ToString() | Set-Content -LiteralPath (Join-Path $outputPath 'tool-exit-code.txt') -Encoding ASCII
            }
            Write-Host "Validation exit code: $runExitCode. Results and logs: $outputPath"
        }
        catch { Write-Warning "Could not export runner logs; original logs remain at ${logRoot}: $_" }
    }
    if ($lockHeld -and $null -ne $runLock) { $runLock.ReleaseMutex() }
    if ($null -ne $runLock) { $runLock.Dispose() }
}

exit $runExitCode
