# eam:// on macOS

A drive address is `https://everythingabout.me/files/<path>`, and `eam:///files/<path>`
is the same address with a shorter prefix. There is no account name in either: the session
decides whose tree the address opens. This folder builds the app that makes the short
form work from Finder, a terminal, notes, or any other Mac app.

## Why this exists at all

A website cannot register an operating-system URL scheme. The HTML API a browser offers
(`navigator.registerProtocolHandler`) only accepts `mailto:`/`tel:`-style safelisted schemes
or a custom one prefixed with `web+`, and even then it registers the *browser*, not the OS:
a `web+eam://` link typed into that browser opens the drive, a `web+eam://` link clicked in
Finder still opens nothing.

The OS resolves a scheme through LaunchServices, which only knows bundles that declare
`CFBundleURLTypes`. So `eam://` needs this app; there is no web-only version of it.

| Layer | Address | Registered by |
|---|---|---|
| System-wide | `eam:///files/<path>` | `EAM.app` (this folder) |
| In one browser | `web+eam:///files/<path>` | Settings → Connect → Register, or the manifest |
| Anywhere | `https://everythingabout.me/files/<path>` | nothing — it just works |

## Build and install

```sh
scripts/eam-scheme/build.sh
cp -R scripts/eam-scheme/EAM.app /Applications/
open /Applications/EAM.app        # first run registers the scheme, then it exits
open 'eam:///files/'              # should open your drive
```

Requires the Xcode command line tools (`xcode-select --install`) for `swiftc`.

If another app already owns `eam://`, macOS keeps that handler. Take it over with:

```sh
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister \
  -f /Applications/EAM.app
```

## What it does

Takes the URL, drops `eam://`, and opens `https://everythingabout.me/` + the rest in the
default browser. `eam://f/<id>` therefore opens the same stable entry link as
`https://everythingabout.me/f/<id>`. Nothing else: no window, no dock icon, no network
access of its own.

The base address is one constant in `main.swift`; change it there if the drive moves.
