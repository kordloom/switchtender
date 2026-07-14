<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Railwarden" width="140">
  </picture>
</p>

# Desktop

Railwarden runs as a local desktop application with one command. Because the whole product is a
single binary with an embedded web UI, there is nothing to install alongside it: no database server,
no container, no Kubernetes.

## Download

Every release attaches ready-to-run downloads on the
[releases page](https://github.com/dcadolph/railwarden/releases): a `Railwarden.dmg` for macOS with
the app inside, a `windows_amd64.zip` holding `railwarden.exe`, and `tar.gz` archives of the binary
for macOS and Linux. A `SHA256SUMS` file lists the checksum of each one.

The downloads are not yet signed with a developer certificate, so the operating system warns that
the developer is unidentified on first launch. On macOS, right-click the app and choose Open, then
Open again. On Windows, choose More info and then Run anyway. A signed release removes the warning
and is planned.

## Run it

From a download, open `Railwarden.app` on macOS or run `railwarden.exe desktop` on Windows. From a
binary or a source build, run:

```
railwarden desktop
```

This serves on a private loopback port, keeps its database in a per-user data directory, and opens
the web UI in your default browser. There are no flags to set. The data directory follows the
platform convention:

| Platform | Data directory |
|----------|----------------|
| macOS | `~/Library/Application Support/Railwarden` |
| Windows | `%AppData%\Railwarden` |
| Linux | `~/.config/Railwarden` |

The chosen port persists in the data directory and is reused on the next launch, so your sign-in
and tour state survive a restart. Launching a second copy while one is running opens the existing
instance instead of starting another server.

Set `RAILWARDEN_DESKTOP_NO_BROWSER` to any value to skip opening a browser, for a headless or
remote machine. The encryption pair still enables stored credentials the same way it does for
`serve`.

## Access and tokens

A fresh desktop instance starts with no tokens, and until the first token exists the API answers
without authentication. The server listens only on the loopback interface, so nothing off your
machine can reach it, but on a shared computer any local account can. If other people use the
machine, mint a token right away and sign in with it:

```
railwarden token new --db "<data directory>/railwarden.db" --name me
```

Once one token exists, every request must authenticate.

## Stop it

The desktop app is the server. Closing the browser tab leaves it running so scheduled work
continues. To stop it, quit the terminal it runs in or end the `railwarden` process. The packaged
macOS app below runs without a Dock icon, so stop it from Activity Monitor or with
`pkill railwarden`.

## Build the app yourself

The release workflow builds the macOS app and the dmg automatically, so most people use the
[download](#download). To build the bundle by hand, wrap the binary so it launches from the Dock or
Finder. The bundle's executable is a small launcher that runs `railwarden desktop`. The launcher is
named `launch`, not `Railwarden`, so it does not collide with the `railwarden` binary on a
case-insensitive macOS filesystem:

```sh
APP=Railwarden.app
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp railwarden "$APP/Contents/MacOS/railwarden"
cat > "$APP/Contents/MacOS/launch" <<'SH'
#!/bin/sh
exec "$(dirname "$0")/railwarden" desktop
SH
chmod +x "$APP/Contents/MacOS/launch"
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Railwarden</string>
  <key>CFBundleIdentifier</key><string>dev.railwarden.desktop</string>
  <key>CFBundleExecutable</key><string>launch</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
</dict></plist>
PLIST
```

Double-clicking `Railwarden.app` now starts the server and opens the UI. To share it beyond your own
machine, sign it with an Apple Developer ID and notarize it, then wrap it in a `.dmg`. Without
signing, macOS shows an unidentified-developer warning on first launch.

## Package a Windows app

On Windows the same binary runs with `railwarden.exe desktop`. Build a signed `.msi` installer with
WiX that installs the binary and a Start-menu shortcut to `railwarden.exe desktop`. Signing needs a
code-signing certificate.

Signing and notarization are the only steps that need credentials you provide. The desktop mode
itself is built in and needs nothing.
