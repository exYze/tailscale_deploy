<#
.SYNOPSIS
  Builds the one-click Tailscale installers for Windows, macOS and Linux.

.DESCRIPTION
  Embeds your Tailscale OAuth client secret (or auth key) and settings into
  self-contained binaries in .\dist. The first time, if Go is not installed, it
  downloads a private copy of Go into .\.tools (nothing is installed system-wide).

.EXAMPLE
  .\build.ps1
  Prompts for the key (hidden input) and tags; other settings come from
  deploy-config.json (or deploy-config.example.json if you have not made one).

.EXAMPLE
  .\build.ps1 -Tags tag:endpoint
  Prompts only for the key.

.EXAMPLE
  .\build.ps1 -NoEmbeddedKey
  Builds binaries with no key inside. Pass it at run time with --authkey or the
  TS_DEPLOY_AUTHKEY environment variable (e.g. from an RMM secret variable).
#>
[CmdletBinding()]
param(
    [string]$AuthKey,
    [string[]]$Tags,
    [string]$ConfigPath,
    [string]$OutDir,
    [switch]$NoEmbeddedKey,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$Root = $PSScriptRoot
if (-not $Root) { $Root = Split-Path -Parent $MyInvocation.MyCommand.Path }
if (-not $ConfigPath) { $ConfigPath = Join-Path $Root 'deploy-config.json' }
if (-not $OutDir) { $OutDir = Join-Path $Root 'dist' }
$OutDir = [IO.Path]::GetFullPath($OutDir)
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
# Windows PowerShell 5.1 otherwise serialises some arrays as {"value":[...],"Count":n}
Remove-TypeData System.Array -ErrorAction SilentlyContinue

$Version   = '0.1.0'
$Src       = Join-Path $Root 'src'
$Embedded  = Join-Path $Src 'embedded_config.json'
$Utf8NoBom = New-Object System.Text.UTF8Encoding $false

function Write-Step([string]$Message) { Write-Host "==> $Message" -ForegroundColor Cyan }

function Test-Interactive {
    if (-not [Environment]::UserInteractive) { return $false }
    foreach ($a in [Environment]::GetCommandLineArgs()) { if ($a -like '-NonI*') { return $false } }
    return $true
}

function Set-Prop($Obj, [string]$Name, $Value) {
    if ($Obj.PSObject.Properties[$Name]) { $Obj.$Name = $Value }
    else { $Obj | Add-Member -NotePropertyName $Name -NotePropertyValue $Value }
}

function Get-Prop($Obj, [string]$Name) {
    if ($Obj.PSObject.Properties[$Name]) { return $Obj.$Name }
    return $null
}

function Get-GoExe {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    $local = Join-Path $Root '.tools\go\bin\go.exe'
    if (Test-Path $local) { return $local }

    Write-Step 'Go not found - downloading a private copy into .tools\ (one time)'
    $hostArch = 'amd64'
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $hostArch = 'arm64' }
    $releases = Invoke-RestMethod 'https://go.dev/dl/?mode=json'
    $rel  = $releases | Where-Object { $_.stable } | Select-Object -First 1
    $file = $rel.files | Where-Object { $_.os -eq 'windows' -and $_.arch -eq $hostArch -and $_.kind -eq 'archive' } | Select-Object -First 1
    if (-not $file) { throw 'Could not find a Go download for this machine.' }
    $zip = Join-Path ([IO.Path]::GetTempPath()) $file.filename
    Invoke-WebRequest -UseBasicParsing -Uri "https://go.dev/dl/$($file.filename)" -OutFile $zip
    $hash = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLower()
    if ($hash -ne $file.sha256) { Remove-Item $zip; throw "Go download checksum mismatch ($hash)" }
    $tools = Join-Path $Root '.tools'
    New-Item -ItemType Directory -Force -Path $tools | Out-Null
    & tar.exe -xf $zip -C $tools
    if ($LASTEXITCODE -ne 0) { throw 'Extracting Go failed.' }
    Remove-Item $zip
    Write-Host "    $($rel.version) installed to $tools\go"
    return $local
}

# ---------------------------------------------------------------- configuration
$example = Join-Path $Root 'deploy-config.example.json'
if (Test-Path -LiteralPath $ConfigPath) {
    Write-Step "Reading settings from $ConfigPath"
    $cfg = Get-Content -Raw -LiteralPath $ConfigPath | ConvertFrom-Json
} elseif (Test-Path -LiteralPath $example) {
    Write-Step "No deploy-config.json found - using defaults from deploy-config.example.json"
    $cfg = Get-Content -Raw -LiteralPath $example | ConvertFrom-Json
} else {
    $cfg = New-Object psobject
}

if ($AuthKey) { Set-Prop $cfg 'authKey' $AuthKey }
if ($Tags)    { Set-Prop $cfg 'tags' @($Tags | ForEach-Object { $_ -split '[,\s]+' } | Where-Object { $_ }) }

if ($NoEmbeddedKey) {
    Set-Prop $cfg 'authKey' ''
} else {
    $key = [string](Get-Prop $cfg 'authKey')
    if (-not $key -or $key -match 'REPLACE') {
        if (-not (Test-Interactive)) {
            throw 'No auth key. Pass -AuthKey, put "authKey" in deploy-config.json, or use -NoEmbeddedKey.'
        }
        Write-Host ''
        Write-Host 'Paste your Tailscale OAuth client secret (tskey-client-...) or auth key (tskey-auth-...).' -ForegroundColor Yellow
        Write-Host 'Input is hidden. It is embedded in the binaries and NOT saved to disk.' -ForegroundColor Yellow
        $sec = Read-Host 'Key' -AsSecureString
        $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($sec)
        try { $key = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr).Trim() }
        finally { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
        Set-Prop $cfg 'authKey' $key
    }
    $tagList = @(Get-Prop $cfg 'tags' | Where-Object { $_ })
    if ($key -like 'tskey-client-*' -and $tagList.Count -eq 0) {
        if (-not (Test-Interactive)) { throw 'OAuth client secrets need at least one tag: pass -Tags tag:endpoint' }
        $t = Read-Host 'Tag(s) to apply to every device (e.g. tag:endpoint)'
        Set-Prop $cfg 'tags' @($t -split '[,\s]+' | Where-Object { $_ })
    }
}

# ---------------------------------------------------------------------- build
$go = Get-GoExe
Write-Step "Using $(& $go version)"

$targets = @(
    @{ GOOS = 'windows'; GOARCH = '386';   Out = 'TailscaleDeploy-Windows.exe' }   # one exe for x64, ARM64 and x86
    @{ GOOS = 'darwin';  GOARCH = 'amd64'; Out = '.darwin-amd64' }
    @{ GOOS = 'darwin';  GOARCH = 'arm64'; Out = '.darwin-arm64' }
    @{ GOOS = 'linux';   GOARCH = 'amd64'; Out = 'TailscaleDeploy-Linux-x86_64' }
    @{ GOOS = 'linux';   GOARCH = 'arm64'; Out = 'TailscaleDeploy-Linux-arm64' }
    @{ GOOS = 'linux';   GOARCH = 'arm';   Out = 'TailscaleDeploy-Linux-arm'; GOARM = '6' }
    @{ GOOS = 'linux';   GOARCH = '386';   Out = 'TailscaleDeploy-Linux-i386' }
)

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
Get-ChildItem -LiteralPath $OutDir -File | Where-Object { $_.Name -like 'TailscaleDeploy-*' -or $_.Name -like '.darwin-*' -or $_.Name -in @('SHA256SUMS.txt', 'Tailscale-macOS-MDM.mobileconfig') } | Remove-Item -Force

$savedEnv = @{}
foreach ($n in 'GOOS', 'GOARCH', 'GOARM', 'CGO_ENABLED') { $savedEnv[$n] = [Environment]::GetEnvironmentVariable($n, 'Process') }
function Restore-GoEnv { foreach ($n in $savedEnv.Keys) { [Environment]::SetEnvironmentVariable($n, $savedEnv[$n], 'Process') } }

Push-Location $Src
try {
    if (-not $SkipTests) {
        Write-Step 'Running unit tests'
        & $go test ./...
        if ($LASTEXITCODE -ne 0) { throw 'Unit tests failed.' }
    }

    $json = $cfg | ConvertTo-Json -Depth 10
    [IO.File]::WriteAllText($Embedded, $json, $Utf8NoBom)

    $env:CGO_ENABLED = '0'
    $ldflags = "-s -w -X main.buildVersion=$Version"
    foreach ($t in $targets) {
        $env:GOOS = $t.GOOS; $env:GOARCH = $t.GOARCH
        if ($t.ContainsKey('GOARM')) { $env:GOARM = $t.GOARM } else { Remove-Item Env:GOARM -ErrorAction SilentlyContinue }
        Write-Step "Building $($t.GOOS)/$($t.GOARCH)"
        & $go build -trimpath -ldflags $ldflags -o (Join-Path $OutDir $t.Out) .
        if ($LASTEXITCODE -ne 0) { throw "Build failed for $($t.GOOS)/$($t.GOARCH)" }
    }
    Restore-GoEnv

    Write-Step 'Creating universal macOS binary (Intel + Apple silicon)'
    $macOut = Join-Path $OutDir 'TailscaleDeploy-macOS'
    & $go run ./tools/machofat $macOut (Join-Path $OutDir '.darwin-amd64') (Join-Path $OutDir '.darwin-arm64')
    if ($LASTEXITCODE -ne 0) { throw 'Creating the universal macOS binary failed.' }
    Remove-Item (Join-Path $OutDir '.darwin-*') -Force
}
finally {
    [IO.File]::WriteAllText($Embedded, "{}`n", $Utf8NoBom)   # never leave the key in the source tree
    Restore-GoEnv
    Pop-Location
}

$winExe = Join-Path $OutDir 'TailscaleDeploy-Windows.exe'
if (-not $NoEmbeddedKey) {
    Write-Step 'Validating the embedded configuration'
    & $winExe --check-config
    if ($LASTEXITCODE -ne 0) { throw 'The embedded configuration is invalid (see above). Fix deploy-config.json and rebuild.' }
}

Copy-Item -LiteralPath (Join-Path $Root 'macos\Tailscale-macOS-MDM.mobileconfig') -Destination $OutDir -Force

$sums = Get-ChildItem -LiteralPath $OutDir -File | Where-Object { $_.Name -ne 'SHA256SUMS.txt' } | Sort-Object Name | ForEach-Object {
    "{0}  {1}" -f (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLower(), $_.Name
}
[IO.File]::WriteAllText((Join-Path $OutDir 'SHA256SUMS.txt'), (($sums -join "`n") + "`n"), $Utf8NoBom)

Write-Host ''
Write-Host "Done. Output in $OutDir" -ForegroundColor Green
Get-ChildItem -LiteralPath $OutDir -File | Sort-Object Name | Format-Table @{ n = 'File'; e = { $_.Name } }, @{ n = 'Size (MB)'; e = { '{0:N1}' -f ($_.Length / 1MB) } } -AutoSize | Out-String | Write-Host
if ($NoEmbeddedKey) {
    Write-Host 'These binaries contain NO key: run them with --authkey <key> or set TS_DEPLOY_AUTHKEY.' -ForegroundColor Yellow
} else {
    Write-Host 'These binaries CONTAIN YOUR KEY. Share them only through channels you trust; revoke the key/OAuth client when rollout is finished.' -ForegroundColor Yellow
}
