# StatusPan for Mac

StatusPan.app is a native window around the local board. On launch it checks `http://<addr>/healthz`. If a board is already running, the window loads it and does not start another daemon. If nothing answers, the app starts the bundled `statuspan serve` and loads the board when it comes up. Quitting the app sends SIGTERM only to a daemon the app itself started.

The address is `STATUSPAN_ADDR` when that is set, otherwise `127.0.0.1:4477`. The daemon inherits the environment, so `STATUSPAN_DATA` still chooses the data directory. Links that leave the board open in the default browser. Daemon output is appended to `~/Library/Logs/StatusPan/daemon.log`.

## Build

From the repo root:

```bash
macos/build.sh 0.0.0-dev
```

That writes an unsigned universal app and disk image:

- `macos/.build/StatusPan.app`
- `macos/.build/StatusPan.dmg`

`macos/build.sh VERSION --sign` signs the bundled daemon, the app, and the disk image. The identity is `APPLE_SIGNING_IDENTITY`. `macos/notarize.sh path/to/StatusPan.dmg` submits the disk image with `APPLE_NOTARYTOOL_PROFILE`, then staples it.

`macos/make-icon.sh` rebuilds `macos/StatusPan.icns` from `site/favicon.svg`. The checked-in icon is that output, so a normal build does not render it.

## Try a throwaway board

`open` does not pass environment variables into the app. Run the executable:

```bash
STATUSPAN_ADDR=127.0.0.1:14478 \
STATUSPAN_DATA="$(mktemp -d)" \
  macos/.build/StatusPan.app/Contents/MacOS/StatusPan
```

That uses port 14478 and its own data directory. Quit the app to stop the daemon it started.
