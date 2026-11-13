[CmdletBinding()]
param(
    [string]$BuildDirectory,
    [string]$CMakeGenerator
)

$ErrorActionPreference = 'Stop'
$serverDirectory = Split-Path -Parent $PSScriptRoot
if (-not $BuildDirectory) {
    $BuildDirectory = Join-Path $serverDirectory '.build/battle'
}
$BuildDirectory = $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($BuildDirectory)

function Invoke-Checked {
    param([string]$Command, [string[]]$Arguments)

    & $Command @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Command failed with exit code $LASTEXITCODE"
    }
}

Push-Location -LiteralPath $serverDirectory
try {
    Invoke-Checked go @('test', './...')
    Invoke-Checked go @('vet', './...')

    $configure = @('-S', 'battle-server', '-B', $BuildDirectory, '-DBUILD_TESTING=ON')
    $cachePath = Join-Path $BuildDirectory 'CMakeCache.txt'
    if (-not $CMakeGenerator -and -not (Test-Path -LiteralPath $cachePath) -and (Get-Command ninja -ErrorAction SilentlyContinue)) {
        $CMakeGenerator = 'Ninja'
    }
    if ($CMakeGenerator) {
        $configure += @('-G', $CMakeGenerator)
    }
    Invoke-Checked cmake $configure
    Invoke-Checked cmake @('--build', $BuildDirectory, '--config', 'Debug')
    Invoke-Checked ctest @('--test-dir', $BuildDirectory, '-C', 'Debug', '--output-on-failure')
}
finally {
    Pop-Location
}
