# Pull WeKnora images from GitHub Container Registry (ghcr.io) and restore
# their original names, so docker-compose.yml works unchanged.
# Run on the VM (Windows) from the project root:
#   powershell -ExecutionPolicy Bypass -File .\scripts\acr-pull.ps1
$ErrorActionPreference = "Stop"

# Resolve project root (parent of this script's folder)
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$registry = if ($env:ACR_REGISTRY) { $env:ACR_REGISTRY } else { "ghcr.io" }
$ns       = if ($env:ACR_NAMESPACE) { $env:ACR_NAMESPACE } else { "zhangyulinbw" }
$repo     = if ($env:ACR_REPO)      { $env:ACR_REPO }      else { "weknora" }
$remote   = "$registry/$ns/$repo"
$manifest = "scripts\acr-images.txt"

if (-not (Test-Path $manifest)) {
    Write-Error "Manifest not found: $manifest (copy it together with the config package)"
}

Write-Host "==> Registry: $remote"
Get-Content $manifest | ForEach-Object {
    $img = $_.Trim()
    if (-not $img) { return }
    $tag = $img -replace '[/:]', '-'
    Write-Host "==> pull $remote`:$tag  ->  $img"
    docker pull "$remote`:$tag"
    if ($LASTEXITCODE -ne 0) { Write-Error "Pull failed: $remote`:$tag" }
    docker tag "$remote`:$tag" $img
    if ($LASTEXITCODE -ne 0) { Write-Error "Tag failed: $img" }
}

Write-Host "==> Done. Verify with: docker images"
Write-Host '==> Then start: docker compose --profile neo4j up -d'
