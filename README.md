# yumiboard-webui

The web store of **YUMI OS Board**: a single static Go binary that fronts the
[Yumi-Lab/pi-apps](https://github.com/Yumi-Lab/pi-apps) engine on Yumi boards
(SmartPad, SmartPi One — armv7l 32-bit). Beginners install and uninstall curated
mini-projects in one click from a browser; the engine does the real work.

## Install

```bash
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o yumiboard .
./yumiboard -dir /home/pi/pi-apps -catalog catalog-v1.txt -port 8080
```

Then open `http://<board>:8080`.

| Flag | Default | Role |
|------|---------|------|
| `-dir` | `/home/pi/pi-apps` | path to the pi-apps checkout (the engine) |
| `-port` | `8080` | HTTP listen port |
| `-catalog` | *(empty)* | optional file with one app name per line; empty = every installable app |
| `-ttyd` | `http://127.0.0.1:7681` | ttyd base URL reverse-proxied under `/terminal/`; empty disables the web terminal |

## Web terminal

The **Terminal** button opens a slide-up panel with a full shell, served by
[ttyd](https://github.com/tsl0922/ttyd) and reverse-proxied under `/terminal/`
on the same port — one origin, no extra exposed service. Start ttyd bound to
localhost before launching this binary:

```bash
ttyd -p 7681 -i 127.0.0.1 -b /terminal -W bash
```

> Security: ttyd's `-W` gives a writable shell with no authentication. Only run
> it on a trusted LAN/VPN, and put authentication in front before shipping it in
> the image.

## How it works

- `GET /api/apps` — catalog with name, description, category and status, read from
  the engine's `apps/*/` metadata and `data/status/*` files.
- `POST /api/action` — starts `./manage install|uninstall <app>` (one job at a
  time, HTTP 409 while busy).
- `GET /api/job?offset=N` — job state plus the log bytes past N; the UI polls it
  every second and streams the output into a bottom drawer.
- `GET /api/icon/<app>` — the app's `icon-64.png`.
- `/` — the embedded single-file UI (`static/index.html`): system fonts, light and
  dark themes, no external requests, no localStorage. State lives server-side only.

## Measured status (SmartPi One, Allwinner H3, Armbian Bookworm armhf)

- Binary size 5.9 MB, RSS at idle 6.5 MB.
- Full install → uninstall cycle validated through real browser clicks
  (USBImager: 30 packages in, 30 packages purged, engine accounting intact).
- Page makes zero requests outside its own host; localStorage stays empty.

Not yet covered: authentication (LAN/VPN use only for now) and running as a
systemd service — both belong to the YumiOS-Board image build.

## License

MIT — see [LICENSE](LICENSE). The pi-apps engine it drives is a separate
GPL-3.0 project.
