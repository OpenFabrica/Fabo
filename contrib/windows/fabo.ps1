# Fabo on Windows: installs the Linux engine into a WSL2 distro, and
# fabo-win.exe, which serves the Docker API on \\.\pipe\fabo for the Windows
# docker CLI (as the `fabo` context) and carries it into WSL, with
# fabo-desktop.exe, the window it opens. It brings the
# docker CLI, Compose and buildx too, as Fabo.app does, so no other Docker
# is needed. Per user: no admin, no service, no change to WSL's or Windows'
# network settings.
#   powershell -ExecutionPolicy Bypass -File fabo.ps1 [-Distro Ubuntu] [-Bundle C:\path\to\fabo-windows-x86_64 (or -aarch64)] [-Default]
#   powershell -ExecutionPolicy Bypass -File fabo.ps1 -Uninstall [-Distro Ubuntu]
#   powershell -ExecutionPolicy Bypass -File fabo.ps1 -Update -Msi <staged .msi> [-WaitFor <pid>]
# The MSI (installer/windows) lays the same files in %LOCALAPPDATA%\Fabo and
# runs `-Uninstall -IfSetUp` when removed: nothing happens unless first-run
# setup completed, and the distro setup chose is taken from setup.json.
# fabo-win.exe runs `-Update` (engine/cmd/fabo-win/update.go) when it quits for an update it
# downloaded: once that process is gone the staged MSI upgrades Fabo
# silently and the new Fabo opens.
# Fabo becomes docker's default engine (the current context, and the builder
# `docker build` uses) unless Docker Desktop is installed; -Default makes it
# the default anyway.
param(
  [string]$Distro = "Ubuntu",
  [string]$Bundle = $PSScriptRoot,
  [switch]$Default,
  [switch]$Uninstall,
  [switch]$IfSetUp,
  [switch]$Update,
  [string]$Msi,
  [int]$WaitFor
)
$ErrorActionPreference = "Stop"

# -Update: waits for the fabo-win that downloaded the MSI to quit (its files
# are in use until then), upgrades silently (per user, no prompt; 3010 is
# "done, restart Windows later"), clears the stage when that went through,
# and opens the new Fabo. Each step goes to helper.log beside the MSI, which
# stays, with msiexec's install.log, when the upgrade failed.
if ($Update) {
  if (-not $Msi) { throw "-Update needs -Msi" }
  $stage = Split-Path $Msi -Parent
  $helperLog = Join-Path $stage "helper.log"
  function Say($m) { Add-Content -LiteralPath $helperLog -Value "$(Get-Date -Format s) $m" }
  try {
    Say "waiting for fabo-win $WaitFor"
    while ($WaitFor -and (Get-Process -Id $WaitFor -ErrorAction SilentlyContinue)) { Start-Sleep -Milliseconds 200 }
    Say "installing $Msi"
    $p = Start-Process -FilePath (Join-Path $env:SystemRoot "System32\msiexec.exe") -ArgumentList "/i", "`"$Msi`"", "/qn", "/norestart", "/L*V", "`"$(Join-Path $stage 'install.log')`"" -Wait -PassThru
    Say "msiexec exit $($p.ExitCode)"
    if ($p.ExitCode -eq 0 -or $p.ExitCode -eq 3010) { Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue }
    Start-Process -FilePath (Join-Path $env:LOCALAPPDATA "Fabo\fabo-win.exe")
  } catch { Say "failed: $_" }
  return
}

$setupJson = Join-Path $env:LOCALAPPDATA "Fabo\setup.json"
if ($IfSetUp -and -not (Test-Path $setupJson)) { return }
if (-not $PSBoundParameters.ContainsKey("Distro") -and (Test-Path $setupJson)) {
  $saved = (Get-Content $setupJson -Raw | ConvertFrom-Json).distro
  if ($saved) { $Distro = $saved }
}
if (-not (Get-Command wsl.exe -ErrorAction SilentlyContinue)) { throw "WSL is not installed: run 'wsl --install' first" }
$dir = Join-Path $env:LOCALAPPDATA "Fabo"
$exe = Join-Path $dir "fabo-win.exe"
$bin = Join-Path $dir "bin"
$plugins = Join-Path $bin "cli-plugins"
$link = Join-Path ([Environment]::GetFolderPath("Startup")) "Fabo.lnk"
$dockerConfig = Join-Path $env:USERPROFILE ".docker\config.json"
$elevated = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)

# Fabo's bin on this user's PATH: last, so a docker CLI already installed
# (Docker Desktop's is on the system PATH, which comes first) keeps winning.
# Through the registry, not [Environment]::SetEnvironmentVariable, which
# would write REG_SZ and turn %USERPROFILE% and the like in the user's PATH
# into fixed paths; the value keeps its kind (REG_EXPAND_SZ) and its text.
function Set-FaboPath([bool]$On) {
  $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey("Environment", $true)
  try {
    $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
    $raw = ""
    if ($key.GetValueNames() -contains "Path") {
      $kind = $key.GetValueKind("Path")
      $raw = [string]$key.GetValue("Path", "", [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    }
    $parts = @(($raw -split ";") | Where-Object { $_ -and $_ -ne $bin -and ([Environment]::ExpandEnvironmentVariables($_) -ne $bin) })
    if ($On) { $parts += $bin }
    if ($parts.Count) { $key.SetValue("Path", ($parts -join ";"), $kind) } else { $key.DeleteValue("Path", $false) }
  } finally { $key.Close() }
  # What SetEnvironmentVariable would have done: tell Explorer and the rest
  # that the environment changed, so new terminals see the new PATH.
  if (-not ("Fabo.Env" -as [type])) {
    Add-Type -Namespace Fabo -Name Env -MemberDefinition '[DllImport("user32.dll", CharSet = CharSet.Unicode)] public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint msg, UIntPtr wParam, string lParam, uint flags, uint timeout, out UIntPtr result);'
  }
  $r = [UIntPtr]::Zero
  [Fabo.Env]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, "Environment", 2, 5000, [ref]$r) | Out-Null
  $env:Path = (@(($env:Path -split ";") | Where-Object { $_ -and $_ -ne $bin }) + $(if ($On) { $bin } else { @() })) -join ";"
}

# Compose and buildx through the docker config's cliPluginsExtraDirs. The
# config also holds registry logins: it is merged, never replaced, and
# backed up once before Fabo first changes it.
function Set-FaboPlugins([bool]$On) {
  $cfg = if (Test-Path $dockerConfig) { Get-Content $dockerConfig -Raw | ConvertFrom-Json } else { [pscustomobject]@{} }
  $dirs = @($cfg.cliPluginsExtraDirs | Where-Object { $_ -and $_ -ne $plugins })
  if ($On) { $dirs += $plugins }
  if ($null -eq $cfg.cliPluginsExtraDirs -and -not $On) { return }
  if ((Test-Path $dockerConfig) -and -not (Test-Path "$dockerConfig.before-fabo")) { Copy-Item $dockerConfig "$dockerConfig.before-fabo" }
  if ($dirs.Count) { $cfg | Add-Member -NotePropertyName cliPluginsExtraDirs -NotePropertyValue $dirs -Force }
  else { $cfg.PSObject.Properties.Remove("cliPluginsExtraDirs") }
  New-Item -ItemType Directory -Force (Split-Path $dockerConfig) | Out-Null
  # UTF-8 without a BOM: Set-Content -Encoding utf8 writes one in Windows
  # PowerShell, and docker then rejects the whole file, logins included.
  [IO.File]::WriteAllText($dockerConfig, ($cfg | ConvertTo-Json -Depth 32), (New-Object Text.UTF8Encoding $false))
  $dockerExe = Join-Path $bin "docker.exe"
  if (-not (Test-Path $dockerExe)) { $dockerExe = (Get-Command docker.exe -ErrorAction SilentlyContinue).Source }
  if ($dockerExe) {
    $prev = $ErrorActionPreference; $ErrorActionPreference = "Continue"
    $out = (& $dockerExe context ls 2>&1 | ForEach-Object { "$_" }) -join "`n"
    $ErrorActionPreference = $prev
    if ($out -match "Error parsing config") { throw "docker cannot read $dockerConfig after Fabo changed it (the backup is $dockerConfig.before-fabo): $out" }
  }
}

# A WSL distro's folder (the registry has what `wsl --list` prints as UTF-16).
function Get-WslBasePath([string]$Name) {
  foreach ($k in Get-ChildItem "HKCU:\Software\Microsoft\Windows\CurrentVersion\Lxss" -ErrorAction SilentlyContinue) {
    $p = Get-ItemProperty $k.PSPath -ErrorAction SilentlyContinue
    if ($p.DistributionName -eq $Name) { return ([string]$p.BasePath) -replace '^\\\\\?\\', '' }
  }
}

Get-Process fabo-win, fabo-desktop -ErrorAction SilentlyContinue | Stop-Process -Force
if ($Uninstall) {
  if ($IfSetUp) {
    # From the MSI: %LOCALAPPDATA%\Fabo is about to go, so the log lives in %TEMP%.
    Start-Transcript -Path (Join-Path $env:TEMP "fabo-uninstall.log") -Force | Out-Null
    "PowerShell $PSHOME ($([IntPtr]::Size * 8)-bit), elevated: $elevated, interactive: $([Environment]::UserInteractive), distro: $Distro"
    if ($elevated) {
      Write-Host "Fabo: not undoing setup from an elevated session (it would act on the administrator's WSL and docker settings, not yours): uninstall from a non-elevated session"
      Stop-Transcript | Out-Null
      exit 1
    }
  }
  # Each step on its own: one that fails (a docker CLI that is gone, WSL not
  # answering) must not keep the next from running.
  $ErrorActionPreference = "Continue"
  $problems = @()
  function Step([string]$What, [scriptblock]$Do) {
    try { & $Do; Write-Host "- $What" } catch { Write-Host "- $What : FAILED: $_"; $script:problems += $What }
  }
  # Uninstalling stops everything Fabo runs: fabo-win is gone (above); any
  # fabo or docker command still running from Fabo's folder (`fabo share`, a
  # docker compose up) goes too, before the engine in WSL stops (below).
  Step "Fabo commands still running stopped" {
    Get-Process fabo, docker, docker-compose, docker-buildx -ErrorAction SilentlyContinue |
      Where-Object { $_.Path -and $_.Path.StartsWith($dir, [StringComparison]::OrdinalIgnoreCase) } |
      Stop-Process -Force
  }
  # Before the builder goes (`buildx rm` rewrites buildx's current
  # builder): docker build's own builder and the `builder` alias back as
  # they were before "Build with Fabo" took them, from fabo-win
  # (engine/internal/buildwith). Its state is in $dir, which goes below.
  Step "docker build given back its own builder" {
    if (Test-Path $exe) { $p = Start-Process $exe -ArgumentList "--uninstall-builds" -Wait -PassThru; if ($p.ExitCode -ne 0) { throw "fabo-win exited $($p.ExitCode); see $dir\fabo-win.log" } }
  }
  # Fabo on Windows writes no kubectl context of its own (`fabo k8s` runs
  # in WSL), so there is none to give back here.
  Step "docker context and builder removed" {
    $docker = Join-Path $bin "docker.exe"
    if (-not (Test-Path $docker)) { $docker = (Get-Command docker.exe -ErrorAction SilentlyContinue).Source }
    if ($docker) {
      if ((& $docker context show 2>$null) -eq "fabo") { & $docker context use default 2>$null | Out-Null }
      & $docker buildx rm fabo-build 2>$null | Out-Null
      & $docker context rm -f fabo 2>$null | Out-Null
    }
    if ([Environment]::GetEnvironmentVariable("BUILDX_BUILDER", "User") -eq "fabo-build") { [Environment]::SetEnvironmentVariable("BUILDX_BUILDER", $null, "User") }
  }
  Step "Compose and buildx taken out of $dockerConfig" { Set-FaboPlugins $false }
  Step "$bin taken off PATH" { Set-FaboPath $false }
  # Both ways it was ever set: the Startup-folder shortcut, and the Run
  # value setup's "Start Fabo when I sign in" writes (engine/cmd/fabo-win).
  Step "start at sign-in removed" {
    Remove-Item $link -Force -ErrorAction SilentlyContinue
    Remove-ItemProperty "HKCU:\Software\Microsoft\Windows\CurrentVersion\Run" -Name "Fabo" -ErrorAction SilentlyContinue
  }
  # The engine in WSL. A distro setup imported under %LOCALAPPDATA%\Fabo\wsl
  # (Fabo Linux as `fabo`, or Ubuntu when `wsl --install` was not available)
  # keeps its disk, with the images and containers, in the folder the MSI
  # deletes next: it is unregistered here, so WSL does not keep a distro
  # whose disk is gone. A distro of the user's own only loses the engine.
  $base = Get-WslBasePath $Distro
  $faboOwned = $base -and $base.TrimEnd("\").StartsWith($dir.TrimEnd("\") + "\", [StringComparison]::OrdinalIgnoreCase)
  if ($Distro -eq "fabo" -or $faboOwned) {
    Step "WSL distro '$Distro' stopped" { wsl.exe --terminate $Distro 2>$null | Out-Null }
    if ($base) {
      Write-Host "The WSL distro '$Distro' is Fabo's (its disk is $base\ext4.vhdx): it is unregistered, and the images, containers and volumes on that disk are deleted with it."
      Step "WSL distro '$Distro' unregistered" { wsl.exe --unregister $Distro 2>$null | Out-Null; if ($LASTEXITCODE -ne 0) { throw "wsl --unregister exited $LASTEXITCODE" } }
    } else {
      Write-Host "The WSL distro '$Distro' is not registered (already gone)."
    }
  } else {
    Step "engine removed from $Distro (systemd unit and /usr/local/bin/fabo*)" {
      wsl.exe -d $Distro -u root sh -c "systemctl disable --now fabo 2>/dev/null; rm -f /etc/systemd/system/fabo.service /usr/local/bin/fabo /usr/local/bin/fabo-init /usr/local/bin/fabo-start /usr/libexec/fabo/fabo-daemon /usr/libexec/fabo/fabo-init"
      if ($LASTEXITCODE -ne 0) { throw "wsl exited $LASTEXITCODE" }
    }
    Write-Host "Its images, containers and volumes stay in /var/lib/fabo and /var/lib/fabo-daemon in $Distro."
  }
  Step "$dir removed" { Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue }
  # Last, since it asks for approval (Windows shows a UAC prompt): the DNS
  # rule for <name>.fabo.local, if it was turned on. Declining it, or a
  # session with no desktop to ask on, leaves only this rule behind.
  $nrpt = Get-DnsClientNrptRule -ErrorAction SilentlyContinue | Where-Object { $_.Namespace -contains ".fabo.local" }
  if ($nrpt) {
    if ([Environment]::UserInteractive) {
      Step "DNS rule for *.fabo.local removed (asked for approval)" {
        $p = Start-Process "$PSHOME\powershell.exe" -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList "-NoProfile", "-Command", "Get-DnsClientNrptRule | Where-Object { `$_.Namespace -contains '.fabo.local' } | Remove-DnsClientNrptRule -Force; Clear-DnsClientCache"
        if ($p.ExitCode -ne 0) { throw "exit code $($p.ExitCode)" }
      }
    } else {
      Write-Host "- DNS rule for *.fabo.local left in place (no desktop to ask for approval on)"
    }
    if (Get-DnsClientNrptRule -ErrorAction SilentlyContinue | Where-Object { $_.Namespace -contains ".fabo.local" }) {
      Write-Host "  To remove it, in an elevated PowerShell:  Get-DnsClientNrptRule | Where-Object { `$_.Namespace -contains '.fabo.local' } | Remove-DnsClientNrptRule -Force"
    }
  }
  if ($problems.Count) { Write-Host "Fabo is removed, with $($problems.Count) step(s) that failed (above)." } else { Write-Host "Fabo is removed." }
  if ($IfSetUp) { Stop-Transcript | Out-Null }
  if ($problems.Count) { exit 1 }
  return
}

$wslPath = (wsl.exe -d $Distro wslpath -a ($Bundle -replace '\\', '/')).Trim()
wsl.exe -d $Distro -u root sh "$wslPath/install.sh"
if ($LASTEXITCODE -ne 0) { throw "install.sh failed in $Distro" }

New-Item -ItemType Directory -Force $dir, $plugins | Out-Null
if ([IO.Path]::GetFullPath($Bundle).TrimEnd("\") -ne [IO.Path]::GetFullPath($dir).TrimEnd("\")) {
  foreach ($file in @("fabo", "fabo-daemon", "fabo-init", "fabo-start", "install.sh", "fabo.ps1", "fabo-desktop.exe", "ghostty-vt.dll")) {
    Copy-Item (Join-Path $Bundle $file) $dir -Force
  }
}
Copy-Item (Join-Path $Bundle "fabo-win.exe") $exe -Force
# What setup would have recorded: `fabo k8s` and the uninstaller read the
# distro from here.
[IO.File]::WriteAllText($setupJson, (@{ done = $true; linux = "ubuntu"; distro = $Distro } | ConvertTo-Json), (New-Object Text.UTF8Encoding $false))
Copy-Item (Join-Path $Bundle "bin\docker.exe"), (Join-Path $Bundle "bin\fabo.exe") $bin -Force
Copy-Item (Join-Path $Bundle "bin\cli-plugins\*.exe") $plugins -Force
Set-FaboPath $true
Set-FaboPlugins $true

# Started at login, for this user only, in the tray.
$shell = New-Object -ComObject WScript.Shell
$s = $shell.CreateShortcut($link)
$s.TargetPath = $exe
$s.Arguments = "--background --distro $Distro"
$s.WorkingDirectory = $dir
$s.Save()
Start-Process $exe -ArgumentList "--distro", $Distro

$docker = (Get-Command docker.exe).Source
& $docker context rm -f fabo 2>$null | Out-Null
& $docker context create fabo --description "Fabo (WSL2)" --docker "host=npipe:////./pipe/fabo" | Out-Null
# docker build: BuildKit in a Fabo container, loading what it builds into
# Fabo (fabo-win makes it too, if this finds the engine not up yet).
& $docker buildx inspect fabo-build 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) { & $docker buildx create --name fabo-build --driver docker-container --driver-opt default-load=true fabo 2>$null | Out-Null }

# The default engine. With the `fabo` context in use, plain `docker build`
# builds on Fabo's engine; `docker buildx build --builder fabo-build` uses
# the BuildKit container. Beside Docker Desktop, Docker stays the default.
$desktop = Test-Path (Join-Path $env:ProgramFiles "Docker\Docker\Docker Desktop.exe")
if ($Default -or -not $desktop) {
  & $docker context use fabo | Out-Null
  Write-Host "Fabo is docker's default engine: docker run --rm alpine echo hello"
} else {
  Write-Host "Docker Desktop is installed and stays the default: docker --context fabo run --rm alpine echo hello"
  Write-Host "(to make Fabo the default, run fabo.ps1 -Default; docker buildx build --builder fabo-build builds on Fabo)"
}
Write-Host "(docker: $docker; open a new terminal for PATH to apply)"
