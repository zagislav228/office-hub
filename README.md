# Office Hub

A single-file Windows app: a catalog of 27 free replacements for Microsoft Office and other Microsoft products.
Run `office-hub.exe`, pick what you need, press Install — the software is installed through `winget`.
No Node.js, no Python, no third-party package manager to install first.

The catalog is ordered by how closely each program handles Word, Excel and PowerPoint documents. The most compatible
ones come first, and each card shows a five-step "similarity to MS Office" rating.

Note: the interface is currently in Russian. The catalog data lives in `catalog.json` and is easy to translate.

## Running it

Double-click `office-hub.exe` — the app window opens.

The file needs no installer and does not install itself: the catalog and the UI are embedded in the binary, and it
creates nothing next to itself. Keep it in Downloads, in any folder or on a USB stick; to remove it, delete the file.
WebView2 keeps its cache in `%LOCALAPPDATA%\OfficeHub`.

From a terminal:

```bat
office-hub.exe            :: normal run
office-hub.exe --dry      :: prints the winget command but installs nothing
```

## What you get in the window

- a sidebar with categories and counts: office, documents, notes, PDF, graphics and diagrams, mail and data, cloud, media and archives
- search across names, descriptions and file formats; sorting by "most similar to MS Office" or alphabetically
- one card per program: what it replaces, what it does, license, supported formats and the caveats
- the installed version is shown right on the card — the list is checked automatically at startup and after every install
- install folder selection: a path field, a Browse button (native folder picker) and quick presets — `%LOCALAPPDATA%\Programs`,
  `C:\Program Files`, folders on other drives. The path is passed to `winget --location`, so it works only for packages whose
  installer supports a custom location; the rest install to their default place and winget says so in the output panel
- install one at a time or in bulk ("add to plan" → "Install selected"), plus an Update button for installed software
- a `winget` output panel with a live log and the ability to cancel a running job
- a "Silent install" switch that adds `--silent`
- keyboard shortcuts: `/` focuses search, `Esc` clears it
- a separate section, "Official ways to get Microsoft Office": the free web version of Office, checking whether your school
  or employer already provides Microsoft 365, and installing web Word as a PWA through Edge. These are links and instructions,
  not installs — the app contains no cracked builds, keygens or license keys, and never will

## Requirements

- Windows 10 or 11
- `winget` (App Installer) — present on all current Windows builds
- WebView2 Runtime — built into Windows 11; if it is missing, the app says so and points to Microsoft Edge

## Building from source

Go 1.27 or newer is needed to build (end users do not need Go):

```bat
go build -trimpath -ldflags "-s -w -H=windowsgui" -o office-hub.exe .
```

`-H=windowsgui` removes the console window; the catalog and the UI are compiled into the binary with `go:embed`.

## Security

- the app server binds to `127.0.0.1` on a random port and is not reachable from the network
- installation is possible only by an ID present in the catalog: no arbitrary command can be passed through the UI, and
  `winget` is executed directly, without a shell
- the WebView2 cache goes to `%LOCALAPPDATA%\OfficeHub`, not into the app folder
- installers that need administrator rights still raise a UAC prompt — the app never elevates silently

## File formats and associations

`.docx`, `.xlsx` and `.pptx` are the open ECMA-376 specification, and all four office suites in the catalog read and write them.
ONLYOFFICE and WPS Office preserve layout most faithfully (it is their native format), followed by FreeOffice and LibreOffice
(which stores in ODF by default and switches to Microsoft formats via a setting). VBA macros, Word form fields and document-wide
numbering are what survives the move worst.

The app cannot make a new office suite the default handler for `.docx`: Windows 11 does not let third-party programs change
file associations without a person doing it. The UI links to `ms-settings:defaultapps` instead — one manual choice, once.
