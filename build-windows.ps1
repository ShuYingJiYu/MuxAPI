[CmdletBinding()]
param()

$ErrorActionPreference = "Stop"
$projectRoot = $PSScriptRoot
$webRoot = Join-Path $projectRoot "web"
$outputPath = Join-Path $projectRoot "build\muxapi-windows-amd64.exe"

$previousGoos = $env:GOOS
$previousGoarch = $env:GOARCH
$previousCgoEnabled = $env:CGO_ENABLED

try {
    if (-not (Test-Path (Join-Path $webRoot "node_modules"))) {
        Push-Location $webRoot
        try {
            npm ci
            if ($LASTEXITCODE -ne 0) { throw "npm ci failed" }
        }
        finally {
            Pop-Location
        }
    }

    Push-Location $webRoot
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) { throw "frontend build failed" }
    }
    finally {
        Pop-Location
    }

    New-Item -ItemType Directory -Force (Split-Path $outputPath) | Out-Null
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"

    Push-Location $projectRoot
    try {
        go build -trimpath '-ldflags=-s -w -H=windowsgui' -o $outputPath ./cmd/muxapi
        if ($LASTEXITCODE -ne 0) { throw "Go build failed" }
    }
    finally {
        Pop-Location
    }

    Write-Host "Built Windows executable without a console window: $outputPath"
}
finally {
    $env:GOOS = $previousGoos
    $env:GOARCH = $previousGoarch
    $env:CGO_ENABLED = $previousCgoEnabled
}
