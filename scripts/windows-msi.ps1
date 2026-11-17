# The Windows installer: target\windows\Fabo-<version>-<arch>.msi from the
# bundle scripts/windows-bundle.sh made (on any OS; copy target\windows here
# if it was built elsewhere), and the update feed fabo-win reads,
# target\windows\latest-windows-<arch>.json. Windows only: WiX builds MSIs
# with Windows Installer's own libraries. docs/windows.md says how to get
# WiX (`dotnet tool install --global wix`, plus its UI and Util extensions).
#
#   powershell -ExecutionPolicy Bypass -File scripts\windows-msi.ps1 -Version 0.6.0 [-Arch x86_64|arm64]
#
# FABO_RELEASE_BASE is where the MSI will be published, for the feed's url
# (default https://github.com/OpenFabrica/Fabo/releases/download/v<version>).
param(
  [Parameter(Mandatory = $true)][string]$Version,
  [ValidateSet('x86_64', 'arm64')][string]$Arch = 'x86_64'
)
$ErrorActionPreference = 'Stop'
$root = Resolve-Path (Join-Path $PSScriptRoot '..')
$bundle = Join-Path $root "target\windows\fabo-windows-$Arch"
if (-not (Test-Path (Join-Path $bundle 'fabo-win.exe'))) {
  throw "no bundle in $bundle: run scripts/windows-bundle.sh $Arch $Version first"
}
if (-not (Get-Command wix -ErrorAction SilentlyContinue)) {
  throw "wix is not on PATH: dotnet tool install --global wix (see docs/windows.md)"
}
$wixArch = if ($Arch -eq 'arm64') { 'arm64' } else { 'x64' }
# Windows Installer takes numbers only: 0.6.0-rc.1 is 0.6.0.
$msiVersion = ($Version -split '-')[0]
$name = "Fabo-$Version-$Arch.msi"
$out = Join-Path $root "target\windows\$name"

# The dialog art with this version stamped on a copy; the committed,
# unstamped bitmaps serve if that fails.
$assets = Join-Path $root 'target\windows\wix-assets'
$stamp = Join-Path $root 'brand\installer\windows\make-bitmaps.ps1'
$drawn = $true
try { & powershell -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $stamp -Stamp -Version $Version -Out $assets; if ($LASTEXITCODE) { $drawn = $false } } catch { $drawn = $false }
if (-not $drawn) { Write-Warning 'make-bitmaps.ps1 -Stamp failed; using the committed installer bitmaps as they are' }

# The ready page says what the install takes, in the MB Apps & features uses.
$bytes = (Get-ChildItem -Recurse -File $bundle | Measure-Object -Sum Length).Sum
$size = '{0:N1} MB' -f ($bytes / 1MB)

$wixArgs = @('build', '-arch', $wixArch, '-ext', 'WixToolset.UI.wixext', '-ext', 'WixToolset.Util.wixext',
  '-d', "Version=$msiVersion", '-d', "Bundle=$bundle", '-d', "Size=$size")
if ($drawn) { $wixArgs += @('-d', "Assets=$assets") }
$wixArgs += @('-loc', 'fabo.wxl', '-o', $out, 'fabo.wxs')
Push-Location (Join-Path $root 'installer\windows')
try { & wix @wixArgs; if ($LASTEXITCODE) { throw "wix build failed ($LASTEXITCODE)" } } finally { Pop-Location }

# The feed entry fabo-win's updater reads (engine/cmd/fabo-win/update.go).
$base = if ($env:FABO_RELEASE_BASE) { $env:FABO_RELEASE_BASE.TrimEnd('/') } else { "https://github.com/OpenFabrica/Fabo/releases/download/v$Version" }
$sum = (Get-FileHash -Algorithm SHA256 -LiteralPath $out).Hash.ToLower()
$feed = [ordered]@{ version = $Version; url = "$base/$name"; sha256 = $sum; size = (Get-Item $out).Length }
$feedPath = Join-Path $root "target\windows\latest-windows-$Arch.json"
[IO.File]::WriteAllText($feedPath, ($feed | ConvertTo-Json -Compress) + "`n", (New-Object Text.UTF8Encoding $false))
"$out ($([math]::Round((Get-Item $out).Length / 1MB, 1)) MB)"
$feedPath
