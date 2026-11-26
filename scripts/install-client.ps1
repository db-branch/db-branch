# Standalone db-branch client CLI installer for Windows (PowerShell).
# Usage: irm https://raw.githubusercontent.com/db-branch/db-branch/main/scripts/install-client.ps1 | iex

$ErrorActionPreference = 'Stop'

try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls13 -bor [Net.SecurityProtocolType]::Tls12
} catch {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
}

$releaseBase = if ($env:DB_BRANCH_RELEASE_BASE) { $env:DB_BRANCH_RELEASE_BASE.TrimEnd('/') } else { 'https://github.com/db-branch/db-branch/releases' }
$requestedVersion = $env:DB_BRANCH_VERSION

$arch = $null
try {
    $osArch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
    if ($osArch -eq 'X64') { $arch = 'amd64' }
    elseif ($osArch -eq 'Arm64') { $arch = 'arm64' }
} catch {}
if (-not $arch) {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "Unsupported architecture: $env:PROCESSOR_ARCHITECTURE (supported: amd64, arm64)" }
    }
}

if ($requestedVersion) {
    $ver = $requestedVersion.TrimStart('v')
    if ($ver -notmatch '^[A-Za-z0-9._+-]+$') {
        throw "Invalid DB_BRANCH_VERSION: $requestedVersion"
    }
    $downloadBase = "$releaseBase/download/v$ver"
} else {
    $downloadBase = "$releaseBase/latest/download"
}

if ($env:DB_BRANCH_INSTALL_DIR) {
    $installDir = $env:DB_BRANCH_INSTALL_DIR
} elseif ($env:LOCALAPPDATA) {
    $installDir = Join-Path $env:LOCALAPPDATA 'db-branch\bin'
} else {
    throw 'LOCALAPPDATA is not set; set DB_BRANCH_INSTALL_DIR'
}

$runId = [Guid]::NewGuid().ToString('N')
$tmpDir = Join-Path ([IO.Path]::GetTempPath()) ("db-branch-install-" + $runId)
New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

$stagedExe = $null
$backupExe = $null
try {
    $sumsPath = Join-Path $tmpDir 'SHA256SUMS'
    Write-Host "Downloading checksums from $downloadBase/SHA256SUMS..."
    Invoke-WebRequest -UseBasicParsing -Uri "$downloadBase/SHA256SUMS" -OutFile $sumsPath

    $suffix = "_windows_${arch}.zip"
    $matchesList = @()
    foreach ($line in (Get-Content -Path $sumsPath)) {
        if ($line -match '^([0-9a-fA-F]{64})\s+\*?(\S+)$') {
            $sha = $Matches[1].ToLowerInvariant()
            $fname = $Matches[2]
            if ($fname.StartsWith('db-branch_') -and $fname.EndsWith($suffix)) {
                $matchesList += @{ Sha = $sha; Name = $fname }
            }
        }
    }

    if ($matchesList.Count -ne 1) {
        throw "Expected 1 archive matching *$suffix in SHA256SUMS, found $($matchesList.Count)"
    }

    $expectedSha = $matchesList[0].Sha
    $archiveName = $matchesList[0].Name
    if ($archiveName -notmatch '^[A-Za-z0-9._+-]+$' -or $archiveName -match '\.\.') {
        throw "Unsafe archive filename in SHA256SUMS: $archiveName"
    }

    $archivePath = Join-Path $tmpDir $archiveName
    Write-Host "Downloading $archiveName..."
    Invoke-WebRequest -UseBasicParsing -Uri "$downloadBase/$archiveName" -OutFile $archivePath

    $actualSha = (Get-FileHash -Algorithm SHA256 -Path $archivePath).Hash.ToLowerInvariant()
    if ($actualSha -ne $expectedSha) {
        throw "SHA-256 mismatch for ${archiveName}: expected $expectedSha, got $actualSha"
    }

    $extractDir = Join-Path $tmpDir 'extract'
    Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force
    $exeSrc = Join-Path $extractDir 'db-branch.exe'
    if (-not (Test-Path -Path $exeSrc -PathType Leaf)) {
        throw 'db-branch.exe not found in archive'
    }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    $exeDst = Join-Path $installDir 'db-branch.exe'
    $stagedExe = Join-Path $installDir (".db-branch.exe.tmp." + $runId)
    $backupExe = Join-Path $installDir (".db-branch.exe.old." + $runId)

    Copy-Item -Path $exeSrc -Destination $stagedExe -Force
    $movedBackup = $false
    try {
        if (Test-Path $exeDst) {
            Move-Item -Path $exeDst -Destination $backupExe -Force
            $movedBackup = $true
        }
        Move-Item -Path $stagedExe -Destination $exeDst -Force
        $stagedExe = $null
        if ($movedBackup -and (Test-Path $backupExe)) {
            Remove-Item -Path $backupExe -Force -ErrorAction SilentlyContinue
        }
    } catch {
        if ($movedBackup -and (Test-Path $backupExe) -and -not (Test-Path $exeDst)) {
            Move-Item -Path $backupExe -Destination $exeDst -Force -ErrorAction SilentlyContinue
        }
        throw
    }

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $parts = @()
    if ($userPath) { $parts = $userPath -split ';' | Where-Object { $_ -ne '' } }
    if (-not ($parts | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') })) {
        $newPath = (($parts + $installDir) -join ';')
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        $env:Path = "$env:Path;$installDir"
        Write-Host "Added $installDir to your user PATH (restart existing shells if needed)."
    }

    Write-Host "Installed db-branch to $exeDst"
    try { & $exeDst version } catch {}
} finally {
    if ($stagedExe) { Remove-Item -Path $stagedExe -Force -ErrorAction SilentlyContinue }
    if ($backupExe) { Remove-Item -Path $backupExe -Force -ErrorAction SilentlyContinue }
    Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
}
