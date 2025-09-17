# bambu‑lan‑print-recording

A small Go service that connects directly to a **Bambu Lab** printer’s **MQTT** broker (LAN mode), watches the **`device/<DEVICE_UUID>/report`** topic, and records the printer’s **RTSP(S)** camera stream to MP4 **without re‑encoding** while a print is running.

- Starts recording when **`print.gcode_state` = `RUNNING`**  
- Stops and finalizes the MP4 when **`gcode_state` = `FINISH`** or **`FAILED`**
- Uses **`print.ipcam.rtsp_url`** for the stream URL and **`print.subtask_name`** for the filename suffix
- Injects RTSP basic auth (username/password) using the **same LAN‑mode credentials** as MQTT
- Clean, robust finalization of MP4 using `ffmpeg` (stream copy), with graceful shutdown on SIGINT/SIGTERM
- Avoids having to move recordings from USB storage by recording printing stream directly to your NAS or larger hard drives.

---

## Table of contents

- [Why this project?](#why-this-project)
- [How it works](#how-it-works)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration (environment variables)](#configuration-environment-variables)
- [Build from source](#build-from-source)
- [Run as a service (systemd)](#run-as-a-service-systemd)
- [Docker (optional)](#docker-optional)
- [Filename format & sanitization](#filename-format--sanitization)
- [RTSP authentication handling](#rtsp-authentication-handling)
- [Graceful shutdown & MP4 finalization](#graceful-shutdown--mp4-finalization)
- [Troubleshooting](#troubleshooting)
- [Security notes](#security-notes)
- [FAQ](#faq)
- [Development](#development)
- [License](#license)

---

## Why this project?

- **Zero Home Assistant dependency**: Subscribe **directly** to the printer’s MQTT broker instead of polling a third‑party system.
- **Reliable MP4s**: Uses `ffmpeg` with stream‑copy remux (`-c copy`) and graceful termination to ensure the MP4 recording is written correctly.
- **Remote Recording**:  Record your printing sessions directly where your long-term video storage lives vs. on the local USB drive

---

## How it works

- The printer when in LAN-Only Developer Mode publishes an MQTT JSON payload on **`device/<DEVICE_UUID>/report`** (MQTTS, port **8883**).
- This service parses a small subset of that payload:
  - **`print.gcode_state`**: one of `RUNNING`, `FAILED`, `FINISH`
  - **`print.ipcam.rtsp_url`**: base camera URL (e.g., `rtsps://<ip>:322/streaming/live/1`)
  - **`print.subtask_name`**: job name for the filename suffix
- On **`RUNNING`**, we:
  - Insert MQTT credentials (username = typically `bblp`, password = LAN access code) into the RTSP URL.
  - Start a local `ffmpeg` process with `-c copy` to write `YYYY-MM-DD-HHMMSS-<subtask_name>.mp4` to `OUTPUT_DIR`.
- On **`FINISH`** or **`FAILED`**, we:
  - Send `q
` to `ffmpeg` stdin so it cleanly writes the MP4 trailer (and optional `faststart` second pass).
  - Wait up to `STOP_GRACE_SECONDS` for finalization, then escalate if necessary.

**Example subset of the report payload:**
```json
{
  "print": {
    "gcode_state": "RUNNING",
    "subtask_name": "Seeed_Studio_XIAO_ESP32-C6",
    "ipcam": {
      "rtsp_url": "rtsps://10.13.0.133:322/streaming/live/1"
    }
  }
}
```

---

## Requirements

- **Operating system**: Linux (x86_64/arm64). macOS and Windows should also work but are less tested.
- **Go**: 1.21+ recommended
- **ffmpeg**: Available in `PATH` or specify `FFMPEG_PATH`
- **Network**:
  - TCP to the printer on **8883** (MQTTS)
  - TCP to the printer’s **RTSP(S)** port (commonly **322** for Bambu cameras)
- **Printer**:
  - LAN mode enabled
  - Developer Mode
  - LAN access code (used as **MQTT password** and **RTSP password**)

---

## Quick start

```bash
# 1) Build
git clone https://github.com/arodd/bambu-lan-print-recording.git
cd bambu-lan-print-recording
go build -o bambu-lan-print-recording

# 2) Configure environment (example)
export PRINTER_HOST="10.13.0.133"
export MQTT_USERNAME="bblp"
export MQTT_PASSWORD="YOUR_LAN_ACCESS_CODE"
export OUTPUT_DIR="/Datastore/nomad-vols/h2d-print-recordings"
export MQTT_INSECURE_TLS=true          # or provide MQTT_CA_FILE instead
export STOP_GRACE_SECONDS=120
# Optionally disable faststart and use fMP4(not supported by every player but avoids needing to relocate moov information after the session ends)
# export FFMPEG_FASTSTART=false
# export FFMPEG_EXTRA_ARGS="-movflags +frag_keyframe+empty_moov"

# 3) Run
./bambu-lan-print-recording
```

Start a print job. When `gcode_state` changes to `RUNNING`, recording begins. When it changes to `FINISH` or `FAILED`, `ffmpeg` finalizes and exits; the MP4 appears in `OUTPUT_DIR`.

---

## Configuration (environment variables)

| Variable | Required | Default | Description |
|---|---|---:|---|
| `PRINTER_HOST` | ✅ | — | Printer IP/hostname for MQTT & RTSP. |
| `DEVICE_UUID` |  | — | Optionally used to subscribe to `device/<uuid>/report`. If omitted UUID will be automatically discovered. |
| `MQTT_USERNAME` |  | `bblp` | MQTT username (typically `bblp` for Bambu). |
| `MQTT_PASSWORD` | ✅ | — | LAN mode access code. **Also used for RTSP.** |
| `MQTT_PORT` |  | `8883` | MQTT broker TLS port. |
| `MQTT_INSECURE_TLS` |  | `false` | If `true`, skip TLS verification (ok for self‑signed; prefer CA file). |
| `MQTT_CA_FILE` |  | — | PEM CA/cert to verify the broker. If set, verification is enabled. |
| `OUTPUT_DIR` |  | `./recordings` | Directory for MP4 outputs. Will be created if missing. |
| `FFMPEG_PATH` |  | `ffmpeg` | Path to ffmpeg binary. |
| `STOP_GRACE_SECONDS` |  | `120` | How long to wait after sending `q` for ffmpeg to finalize before SIGINT/KILL. |
| `FFMPEG_FASTSTART` |  | `false` | If `true`, add `-movflags +faststart` (rewrites `moov` to front on finalize). |
| `FFMPEG_EXTRA_ARGS` |  | — | Extra ffmpeg args (split by spaces, quotes allowed). |
| `TZ` |  | system local | Timezone for filenames, e.g., `America/Los_Angeles`. |

**Defaults in ffmpeg invocation (always on):**
- `-rtsp_transport tcp` (reduces packet reordering)
- `-fflags +genpts` (generates monotonic PTS)
- `-use_wallclock_as_timestamps 1`
- `-c copy` (no re‑encode)

---

## Build from source

```bash
go mod tidy
go build -o bambu-lan-print-recording
```

Cross‑compile examples:

```bash
# Linux ARM64 (e.g., Raspberry Pi)
GOOS=linux GOARCH=arm64 go build -o bambu-lan-print-recording

# Linux AMD64
GOOS=linux GOARCH=amd64 go build -o bambu-lan-print-recording
```

---

## Run as a service (systemd)

Create `/etc/systemd/system/bambu-lan-print-recording.service`:

```ini
[Unit]
Description=Bambu MQTT RTSP Recorder
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=youruser
Environment="PRINTER_HOST=10.13.0.133"
Environment="DEVICE_UUID=YOUR_DEVICE_UUID"
Environment="MQTT_USERNAME=bblp"
Environment="MQTT_PASSWORD=YOUR_LAN_ACCESS_CODE"
Environment="OUTPUT_DIR=/Datastore/h2d-print-recordings"
Environment="MQTT_INSECURE_TLS=true"
Environment="STOP_GRACE_SECONDS=180"
ExecStart=/usr/local/bin/bambu-lan-print-recording
WorkingDirectory=/Datastore/h2d-print-recordings
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

Then:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now bambu-lan-print-recording
journalctl -u bambu-lan-print-recording -f
```

---

## Docker (optional)

**Dockerfile (example):**
```Dockerfile
FROM golang:1.22-bullseye AS build
WORKDIR /src
COPY . .
RUN go build -o /out/bambu-lan-print-recording

FROM debian:stable-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates   && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/bambu-lan-print-recording /usr/local/bin/bambu-lan-print-recording
VOLUME ["/recordings"]
ENV OUTPUT_DIR=/recordings
ENTRYPOINT ["/usr/local/bin/bambu-lan-print-recording"]
```

**docker-compose.yml (example):**
```yaml
services:
  recorder:
    build: .
    container_name: bambu-recorder
    environment:
      PRINTER_HOST: "10.13.0.133"
      DEVICE_UUID: "YOUR_DEVICE_UUID"
      MQTT_USERNAME: "bblp"
      MQTT_PASSWORD: "YOUR_LAN_ACCESS_CODE"
      OUTPUT_DIR: "/recordings"
      MQTT_INSECURE_TLS: "true"
      STOP_GRACE_SECONDS: "180"
      # FFMPEG_FASTSTART: "false"
      # FFMPEG_EXTRA_ARGS: "-movflags +frag_keyframe+empty_moov"
    volumes:
      - /Datastore/nomad-vols/h2d-print-recordings:/recordings
    network_mode: "host"   # or map ports appropriately if not using host mode
    restart: unless-stopped
```

---

## Filename format & sanitization

Output filenames are:

```
YYYY-MM-DD-HHMMSS-<subtask_name>.mp4
```

- `subtask_name` is sanitized to **letters, digits, `_`, `-`, `.`** only.
- If a file with the same name exists, `-1`, `-2`, … suffixes are appended.
- Timezone comes from `TZ` (or system local).

---

## RTSP authentication handling

- The printer publishes a base RTSP(S) URL (e.g., `rtsps://10.13.0.133:322/streaming/live/1`) **without credentials**.
- This service **injects** LAN‑mode credentials into the URL:
  - **Username**: `MQTT_USERNAME` (default `bblp`)
  - **Password**: `MQTT_PASSWORD` (your LAN access code)
- Logged RTSP URLs are **redacted** (password not printed).

---

## Graceful shutdown & MP4 finalization

- On **SIGINT/SIGTERM** or state change to `FINISH`/`FAILED`:
  1. Send `q
` to `ffmpeg` stdin → `ffmpeg` writes the MP4 trailer  
     (and performs the “second pass” if `FFMPEG_FASTSTART=true`).
  2. Wait up to `STOP_GRACE_SECONDS` for exit.
  3. If still running, send SIGINT; after 10s more, SIGKILL as last resort.
- Finalization time depends on:
  - File size
  - Storage (local SSD vs. network share)
  - Whether `+faststart` is enabled  
- After stop, the service logs the finalized path and size:
  ```
  [recorder] finalized file: /path/2025-09-14-222719-Example.mp4 (4825764 bytes)
  ```

If you don’t need progressive download optimization, leave `FFMPEG_FASTSTART=false` (faster finalization).  
For “instant start” in players without second pass, consider fragmented MP4:

```bash
export FFMPEG_FASTSTART=false
export FFMPEG_EXTRA_ARGS='-movflags +frag_keyframe+empty_moov'
```

---

## Troubleshooting

**No file appears**
- Ensure `ffmpeg` is installed and in `PATH` (`ffmpeg -version`) or set `FFMPEG_PATH`.
- Check write permissions on `OUTPUT_DIR`.
- Confirm `gcode_state` transitions to `RUNNING` (use `mosquitto_sub` to watch payloads).
- Some network filesystems (NFS/SMB) are slow to flush; try increasing `STOP_GRACE_SECONDS`.

**Empty/corrupt MP4**
- Increase `STOP_GRACE_SECONDS` to allow trailer or `faststart` second pass to complete (e.g., `180`–`300`).
- Disable `FFMPEG_FASTSTART` or use fragmented MP4 (see above).
- Check logs for timestamp warnings; we already set `-fflags +genpts` and `-rtsp_transport tcp` to help.

**TLS/connection errors**
- If the printer uses a self‑signed certificate, either:
  - Set `MQTT_INSECURE_TLS=true` **(less secure)**, or
  - Supply a proper CA PEM file via `MQTT_CA_FILE` **(preferred)**.
- Verify the printer is reachable on port `8883`.

**Authentication failures**
- Double‑check the LAN access code and `DEVICE_UUID`.  
- Remember RTSP uses the same credentials; if you changed them, restart the service.

**Recording doesn’t start even though the print is preparing**
- The service starts on **`RUNNING`**, not `PREPARE` or `IDLE`. That’s by design to avoid preheating time.

**Multiple printers**
- Run multiple instances (one per printer), each with its own `PRINTER_HOST`, `DEVICE_UUID`, `OUTPUT_DIR`, etc.

---

## Security notes

- **Secrets**: Store `MQTT_PASSWORD` safely (e.g., systemd `EnvironmentFile`, Docker secrets). Avoid committing it.
- **TLS**: Prefer `MQTT_CA_FILE` over `MQTT_INSECURE_TLS=true`. Insecure mode skips certificate verification.
- **Access control**: Limit who can read the output directory; recordings may contain sensitive visuals.

---

## FAQ

**Why use `ffmpeg` instead of a native Go RTSP→MP4 remuxer?**  
`ffmpeg` handles camera quirks, timing, and MP4 finalization very reliably with minimal flags. Native remuxers exist but require more code to match `ffmpeg`’s robustness across devices.

**Will this re‑encode the video?**  
No. It uses `-c copy` (stream copy), so CPU usage is low and the output is identical to the input bitstream inside an MP4 container.

**Can I change the filename format?**  
Not via env vars yet. It’s easy to customize in `buildOutputFilename()`.

**Does it support audio?**  
If your RTSP carries audio and you want it, you can extend the ffmpeg args to map audio streams as well (the default `-c copy` will copy whatever streams ffmpeg selects by default).

---

## Development

**Project layout (single-binary app):**
```
.
├── main.go
└── README.md
```

**Dependencies:**
- MQTT client: `github.com/eclipse/paho.mqtt.golang`

**Local run with a .env file:**
```bash
cp .env.example .env   # create and edit
set -a; source .env; set +a
go run .              # or: go build && ./bambu-lan-print-recording
```

**Example `.env`**
```bash
PRINTER_HOST=10.13.0.133
DEVICE_UUID=YOUR_DEVICE_UUID
MQTT_USERNAME=bblp
MQTT_PASSWORD=YOUR_LAN_ACCESS_CODE
OUTPUT_DIR=/Datastore/nomad-vols/h2d-print-recordings
MQTT_INSECURE_TLS=true
STOP_GRACE_SECONDS=180
FFMPEG_FASTSTART=false
# FFMPEG_EXTRA_ARGS="-movflags +frag_keyframe+empty_moov"
```

Pull requests and issues welcome!

---

## License

MIT. See `LICENSE` 

---

### Appendix: sample log flow

```
... starting (Go go1.22, PID 12345)
[mqtt] connected to ssl://10.13.0.133:8883
[mqtt] subscribed: device/94A1B2.../report
[event] RUNNING → start: rtsps://bblp:********@10.13.0.133:322/streaming/live/1 => /recordings/2025-09-14-222719-JobName.mp4
[recorder] ffmpeg started (pid=67890), writing to /recordings/...
...
[event] FINISH → stop
[recorder] stopping ffmpeg (pid=67890)...
[recorder] waiting up to 2m0s for ffmpeg to finalize...
[recorder] ffmpeg stopped cleanly (exit=0)
[recorder] finalized file: /recordings/2025-09-14-222719-JobName.mp4 (4825764 bytes)
exiting
```
