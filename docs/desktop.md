<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="../assets/logo-letters-dark.png">
    <img src="../assets/logo-letters.png" alt="Yardmaster" width="140">
  </picture>
</p>

# Desktop

Yardmaster runs as a local desktop application with one command. Because the whole product is a
single binary with an embedded web UI, there is nothing to install alongside it: no database server,
no container, no Kubernetes.

## Run it

```
yardmaster desktop
```

This serves on a private loopback port, keeps its database in a per-user data directory, and opens
the web UI in your default browser. There are no flags to set. The data directory follows the
platform convention:

| Platform | Data directory |
|----------|----------------|
| macOS | `~/Library/Application Support/Yardmaster` |
| Windows | `%AppData%\Yardmaster` |
| Linux | `~/.config/Yardmaster` |

Set `YARDMASTER_DESKTOP_NO_BROWSER` to skip opening a browser, for a headless or remote machine.
The encryption pair still enables stored credentials the same way it does for `serve`.

## Package a macOS app

Wrap the binary in an app bundle so it launches from the Dock or Finder. The bundle's executable is
a small launcher that runs `yardmaster desktop`:

The launcher is named `launch`, not `Yardmaster`, so it does not collide with the `yardmaster`
binary on a case-insensitive macOS filesystem:

```sh
APP=Yardmaster.app
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp yardmaster "$APP/Contents/MacOS/yardmaster"
cat > "$APP/Contents/MacOS/launch" <<'SH'
#!/bin/sh
exec "$(dirname "$0")/yardmaster" desktop
SH
chmod +x "$APP/Contents/MacOS/launch"
cat > "$APP/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>Yardmaster</string>
  <key>CFBundleIdentifier</key><string>dev.yardmaster.desktop</string>
  <key>CFBundleExecutable</key><string>launch</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSUIElement</key><true/>
</dict></plist>
PLIST
```

Double-clicking `Yardmaster.app` now starts the server and opens the UI. To share it beyond your own
machine, sign it with an Apple Developer ID and notarize it, then wrap it in a `.dmg`. Without
signing, macOS shows an unidentified-developer warning on first launch.

## Package a Windows app

On Windows the same binary runs with `yardmaster.exe desktop`. Build a signed `.msi` installer with
WiX that installs the binary and a Start-menu shortcut to `yardmaster.exe desktop`. Signing needs a
code-signing certificate.

Signing and notarization are the only steps that need credentials you provide. The desktop mode
itself is built in and needs nothing.
