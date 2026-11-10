# Tailscale Deploy: one-click install for Windows, macOS & Linux

One self-contained binary per OS. Double-click it (or push it silently from
your RMM/MDM) and it will:

1. Get admin rights: a UAC prompt on Windows, the admin password dialog on macOS, or `sudo`/`pkexec` on Linux. It skips this when it already runs as SYSTEM/root.
2. Check the OS is supported.
3. Download the **official** Tailscale installer, verify its SHA-256, and verify the Tailscale Inc. code signature (Windows Authenticode and macOS Developer ID).
4. Install Tailscale silently.
5. Connect the device to **your** tailnet with the key embedded at build time, tagged and pre-approved, in unattended mode.
6. Report the result in a dialog (when double-clicked) and to a log file, with an exit code for RMM tools.

If the device is already connected, it does nothing and exits 0, so re-running is safe.

| File in `dist\` | Runs on |
|---|---|
| `TailscaleDeploy-Windows.exe` | Windows 10, 11, Server 2016/2019/2022/2025 on **x64, ARM64 and x86**. One exe for all of them. |
| `TailscaleDeploy-macOS` | macOS 13 Ventura and newer, **Intel and Apple silicon** (universal binary) |
| `TailscaleDeploy-Linux-x86_64` / `-arm64` / `-arm` / `-i386` | Any distro: Ubuntu, Debian, RHEL/Rocky/Alma/CentOS, Fedora, Amazon Linux, SUSE, Arch, Alpine, Raspberry Pi OS, ... |
| `Tailscale-macOS-MDM.mobileconfig` | Push from your Mac MDM **before** the binary for zero-click Macs (see below) |
| `SHA256SUMS.txt` | Hashes for EDR/AV allow-listing |

## Prebuilt binaries (no key inside)

[**Releases**](https://github.com/exYze/tailscale_deploy/releases/latest) has ready-made binaries for every OS. They are built with `-NoEmbeddedKey`, so they contain **no key** and are safe to download on any machine. You supply the key when you run them (default tag: `tag:endpoint`, override with `--tags`):

```bash
# From a normal terminal: it asks for admin rights, then installs and connects
TailscaleDeploy-Windows.exe --authkey tskey-client-...
chmod +x TailscaleDeploy-Linux-x86_64 && ./TailscaleDeploy-Linux-x86_64 --authkey tskey-client-...

# Already Administrator/SYSTEM/root (RMM, MDM, admin shell): keep the key off the command line
set TS_DEPLOY_AUTHKEY=tskey-client-...                                  (Windows)
TailscaleDeploy-Windows.exe --silent
sudo env TS_DEPLOY_AUTHKEY=tskey-client-... ./TailscaleDeploy-macOS --silent   (macOS / Linux)
```

Double-clicking a prebuilt binary shows *"no auth key is embedded"*. For true double-click installs, build your own copy with the key embedded (step 2 below) and distribute it privately. **Never upload a key-embedded build to a public release.**

---

## 1. One-time setup in the Tailscale admin console (5 minutes)

**a. Create a tag.** In **Access controls**, add a tag owner, plus a rule for what tagged endpoints may reach. This is an example; adapt it to your network:

```jsonc
"tagOwners": {
  "tag:endpoint": ["autogroup:admin"]
},
"grants": [
  // example: admins can reach every endpoint
  { "src": ["autogroup:admin"], "dst": ["tag:endpoint"], "ip": ["*"] }
]
```

**b. Create the key.** Use an OAuth client secret (recommended): **Settings → Trust credentials → Credential → OAuth**.
Give it the **Auth Keys: Write** scope and the tag `tag:endpoint`. Copy the secret (`tskey-client-...`).

* An OAuth secret **never expires**, so binaries you build keep working for the whole rollout.
* A regular auth key also works (**Settings → Keys**, *Reusable* + *Pre-approved* + tag). But it expires after **at most 90 days**, after which the binaries stop enrolling new devices.
* Tagged devices have **node-key expiry disabled by default**, so endpoints don't drop off the tailnet after 180 days.

## 2. Build your binaries

Double-click **`build.cmd`** (or run `.\build.ps1`). Paste the key when asked (input is hidden and the key is not saved to disk), and enter your tag.
The binaries appear in `dist\`. The first build downloads a private copy of Go into `.tools\`. Nothing is installed system-wide.

```powershell
.\build.ps1 -Tags tag:endpoint                 # prompts for the key
.\build.ps1 -AuthKey tskey-client-... -Tags tag:endpoint
.\build.ps1 -NoEmbeddedKey                     # no key inside; pass it at run time (see "Keyless mode")
```

To change other settings, copy `deploy-config.example.json` to `deploy-config.json` and edit it (see **Settings**).

**Test before rolling out:** run `TailscaleDeploy-Windows.exe --dry-run` on any PC. It checks the OS, then downloads and verifies the installer, but changes nothing. Then pilot on 10-20 devices per OS.

## 3. Roll out

### One click (a person at the device)
Double-click the file → approve the admin prompt → a dialog says **Connected: \<name\> (100.x.y.z) on tailnet \<yours\>**.

### Silently at scale (8000 endpoints)
Run with `--silent` from anything that already runs as **SYSTEM/Administrator/root**. `--silent` never shows dialogs or waits for input.

| Exit code | Meaning | RMM action |
|---|---|---|
| 0 | Connected (or already connected) | success |
| 1 | Failed (see log) | retry / investigate |
| 2 | OS not supported | exclude device |
| 3 | Installed, waiting for approval (macOS without the MDM profile, or a non-pre-approved key) | approve, re-run |
| 4 | Configuration error (missing or invalid key) | rebuild |

**Windows**
* **Intune**: wrap `TailscaleDeploy-Windows.exe` as a Win32 app. Install command `TailscaleDeploy-Windows.exe --silent`, install behavior *System*. Detection rule: file `C:\Program Files\Tailscale\tailscale.exe` exists.
* **Group Policy**: Computer Configuration → Policies → Windows Settings → Scripts → *Startup*. Use `\\server\share\TailscaleDeploy-Windows.exe` with parameter `--silent`, and also enable *Always wait for the network at computer startup and logon*. Domain Computers need read access to the share.
* **SCCM / PDQ / NinjaOne / Datto / ConnectWise / Kaseya / N-able**: run `TailscaleDeploy-Windows.exe --silent` as SYSTEM.

**macOS**
1. Push `Tailscale-macOS-MDM.mobileconfig` to all Macs from your MDM (Jamf, Intune, Kandji/Iru, Mosyle, ...). It pre-approves Tailscale's system extension, VPN configuration and login item. **Without it, macOS requires a person to click "Allow" twice on each Mac.** That is an Apple rule, and only an MDM can bypass it.
2. Run `TailscaleDeploy-macOS --silent` as root from the MDM (e.g. a Jamf policy script: `/path/TailscaleDeploy-macOS --silent`).
   A user must be logged in the first time, because the Tailscale app starts in the user's session. Otherwise it exits with code 3; just re-run it after login.

**Linux**
```bash
# Ansible
ansible all -b -m copy   -a "src=dist/TailscaleDeploy-Linux-x86_64 dest=/tmp/tsdeploy mode=0700"
ansible all -b -m command -a "/tmp/tsdeploy --silent"
# or plain SSH
scp TailscaleDeploy-Linux-x86_64 host:/tmp/tsdeploy && ssh host 'sudo /tmp/tsdeploy --silent'
```
It uses Tailscale's official installer, so the package comes from your distro's package manager and keeps updating. If that isn't possible, it falls back to Tailscale's static binaries plus a systemd service.
Use `-arm64` for ARM servers and Raspberry Pi 4/5 (64-bit OS), and `-arm` for 32-bit Raspberry Pi OS.

---

## "All versions": what is actually supported

Tailscale itself sets these minimums, and this installer follows them:

| OS | Supported | Not possible |
|---|---|---|
| Windows | 10, 11, Server 2016+ (x64/ARM64/x86) | Windows 7, 8, 8.1, Server 2012 R2 and older: current Tailscale doesn't run there |
| macOS | 13 Ventura and newer | macOS 12 and older |
| Linux | Any systemd or OpenRC distro, x86_64/ARM64/ARMv6+/i386 | Systems without systemd/OpenRC still connect, but you must add `tailscaled` to startup yourself (it warns) |

Unsupported machines exit with code **2** and a clear message, so your RMM can report them.

## Security: read this before distributing 8000 copies

* **Anyone who gets a binary can extract the key** and join devices to your tailnet. Mitigations:
  * The key can only create devices with your **tag**. Keep that tag's access rules (`grants`) as narrow as possible.
  * Distribute the binaries only through your RMM/MDM or an internal share. Don't email them or post them publicly.
  * **When rollout is done, revoke the OAuth client** (Settings → Trust credentials). Devices that already joined stay connected; leaked copies stop working.
  * Or use **Keyless mode** (below) so the key lives only in your RMM's secret store.
* The key is never put on a command line and never written to logs. It goes to `tailscale up` through a temporary file that is deleted right away.
* **Code-sign the binaries** for a smooth one-click experience. RMM/MDM pushes don't need signing.
  * Windows: `signtool sign /fd sha256 /tr http://timestamp.digicert.com /td sha256 /a TailscaleDeploy-Windows.exe`. Unsigned downloads show a SmartScreen warning ("More info → Run anyway").
  * macOS: `codesign --force --options runtime --timestamp --sign "Developer ID Application: <Your Org>" TailscaleDeploy-macOS`, then `ditto -c -k TailscaleDeploy-macOS t.zip && xcrun notarytool submit t.zip --wait ...`. Unsigned binaries downloaded in a browser are blocked by Gatekeeper (right-click → Open to bypass).
* **EDR/antivirus:** a new binary that elevates, downloads and installs a VPN can be flagged. Allow-list the hashes in `SHA256SUMS.txt` before rollout.

### Keyless mode
Build with `.\build.ps1 -NoEmbeddedKey`, then supply the key at run time. The environment variable is preferred, because it keeps the key off the command line:
```
set TS_DEPLOY_AUTHKEY=tskey-client-...      (from your RMM's secret variable)
TailscaleDeploy-Windows.exe --silent --tags tag:endpoint
```

## Settings (`deploy-config.json`)

| Key | Default | Meaning |
|---|---|---|
| `authKey` | (prompted) | OAuth client secret or auth key. Leave out of the file to be prompted at build time. |
| `tags` | | Tags to apply, e.g. `["tag:endpoint"]`. Required for OAuth secrets. |
| `oauthPreauthorized` | `true` | Skip device approval for OAuth-enrolled devices |
| `hostnamePrefix` | `""` | e.g. `"corp-"` makes `LAPTOP-123` appear as `corp-laptop-123` |
| `version` | `"latest"` | Or pin, e.g. `"1.104.5"`, for change control |
| `track` | `"stable"` | `"unstable"` for pre-releases |
| `mirrorURL` | `""` | Internal mirror of `pkgs.tailscale.com/stable` (needs a pinned `version`; host the files **and** their `.sha256` files). At 8000 devices × ~40 MB that is ~320 GB you can keep on your LAN. |
| `acceptRoutes` | `false` | Accept subnet routes from other devices |
| `acceptDNS` | Tailscale default | `false` to leave the device's DNS alone |
| `forceReauth` | `false` | Re-enroll devices already logged in to some tailnet (e.g. a user's personal one) |
| `connectTimeoutSeconds` | `180` | How long to wait for the service to start and for the connection |
| `extraUpArgs` | `[]` | Extra `tailscale up` flags, e.g. `["--ssh"]` |
| `windowsMSIProperties` | `TS_UNATTENDEDMODE=always`, `TS_INSTALLUPDATES=always` | Any [Tailscale MSI property](https://tailscale.com/kb/1189/install-windows-msi) |
| `linuxUseInstallScript` | `true` | `false` = always use static binaries |
| `loginServer` | `""` | Custom control server (e.g. Headscale) |

Run-time flags override the embedded settings: `--authkey`, `--tags`, `--hostname-prefix`, `--login-server`, `--force-reauth`. Also: `--dry-run`, `--check-config`, `--version`, `--help`.

## Logs

| OS | Log |
|---|---|
| Windows | `C:\ProgramData\TailscaleDeploy\deploy.log` (MSI log: `msiexec.log` in the same folder) |
| macOS | `/Library/Logs/TailscaleDeploy.log` |
| Linux | `/var/log/tailscale-deploy.log` |

Each run ends with one line your RMM can parse: `RESULT: SUCCESS | Connected: host.tailnet.ts.net (100.x.y.z) on tailnet ...`

## Uninstall / roll back

First, remove the device from the tailnet: `tailscale logout`, or delete it on the admin console's Machines page.

* **Windows:** in PowerShell as admin: `$p = Get-ItemProperty HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\* | ? DisplayName -eq 'Tailscale'; Start-Process msiexec "/x $($p.PSChildName) /qn" -Wait`
* **macOS:** quit Tailscale and move `/Applications/Tailscale.app` to the Trash (macOS then removes its system extension).
* **Linux:** `sudo apt remove tailscale` / `dnf remove tailscale` / etc. For the static-binary fallback: `sudo systemctl disable --now tailscaled && sudo rm /usr/local/bin/tailscale /usr/local/sbin/tailscaled /etc/systemd/system/tailscaled.service`

## Project layout

```
build.cmd / build.ps1         build script (embeds config, cross-compiles, signs nothing)
deploy-config.example.json    settings template
macos/                        MDM pre-approval profile
src/                          Go source (no third-party dependencies)
  main.go, config.go, tailscale.go, util.go, helpers.go   shared logic
  platform_windows.go / platform_darwin.go / platform_linux.go
  tools/machofat/             merges Intel + Apple-silicon builds into one macOS binary
```
Run the tests with `.tools\go\bin\go.exe test ./...` from `src\`.
