# OpusRelay

**Real-time PCM → Opus audio relay.** A small, dependency-free Go server
that takes raw PCM audio over UDP, encodes it to Opus, and broadcasts it to
browsers over WebSocket — with JWT authentication and a native WebCodecs
client.

> **Initial context:** This project was originally developed to enable real-time
> monitoring of radio traffic on a reflector running SVXLink, allowing users
> to listen to radio communications directly in their browser without the
> need for dedicated radio equipment. Over time, it was extended to support
> other generic audio sources, making it a versatile tool for live audio
> streaming.

**[Live demo](https://yo6nam.github.io/OpusRelay/example.html)**

## Why zero external dependencies?

WebSocket implemented natively in the stdlib (RFC 6455), Opus via
CGo directly against libopus — **the only C dependency needed**.

---

## Compatible audio sources (not just svxlink)

The input protocol is completely source-agnostic: `pcmListener` simply
listens on a UDP port for raw PCM samples (16-bit signed, little-endian,
mono, at the configured sample rate — 48kHz by default). There's no
handshake or svxlink-specific framing — anything capable of sending raw PCM
bytes over UDP works.

Examples of alternative sources, all using the same generic `ffmpeg`
command to push PCM over UDP:

```bash
# From any audio file/stream (-re paces the read to real-time — without
# it ffmpeg reads a local file as fast as disk allows and floods the UDP
# listener far ahead of playback)
ffmpeg -re -i <source> -f s16le -ar 48000 -ac 1 udp://127.0.0.1:1235

# From an SDR (e.g. GQRX/SDR++ audio output routed through a virtual
# device, then captured with ffmpeg) — already real-time, no -re needed
ffmpeg -f alsa -i default -f s16le -ar 48000 -ac 1 udp://127.0.0.1:1235

# Relaying an existing Icecast/Shoutcast stream
ffmpeg -re -i https://example.com/stream.mp3 -f s16le -ar 48000 -ac 1 udp://127.0.0.1:1235

# Microphone/line-in directly on the server — already real-time, no -re needed
ffmpeg -f pulse -i default -f s16le -ar 48000 -ac 1 udp://127.0.0.1:1235

# Stereo source, full-bandwidth music profile (start the server with
# -channels 2 -mode music -bitrate 96000; see the flags table above)
ffmpeg -re -i <stereo-source> -f s16le -ar 48000 -ac 2 udp://127.0.0.1:1235
```

> 16 kbps (the default `-bitrate`) is fine for mono voice/PTT traffic, but
> stereo or music content needs a lot more headroom — 64–128 kbps is a
> reasonable range depending on how much you care about fidelity.

Any other tool that can emit raw audio over UDP (GStreamer's `udpsink`,
`sox`, a Python script using `socket`) works just as well. The
`talker_start`/`talker_stop` detection (based on gaps between UDP packets)
is designed for PTT-style traffic (svxlink, Asterisk with VAD) — for a
continuous 24/7 stream, the "someone is talking" indicator fires once and
essentially never returns to "stop"; this doesn't affect the audio itself,
only that one visual indicator in the client.

---

## SVXLink configuration

If your audio source is svxlink itself (rather than one of the generic
`ffmpeg` pipelines above), svxlink can send audio directly to OpusRelay's
UDP listener using its built-in `Multi` transmitter type — no external
piping needed. Add a virtual transmitter that mirrors your existing
logic's audio to `127.0.0.1:1235` (or wherever `-pcmport` points):

```ini
[SimplexLogic]
...
TX=MultiTx

[MultiTx]
TYPE=Multi
TRANSMITTERS=Tx1,TxUDP

[TxUDP]
TYPE=Local
AUDIO_DEV=udp:127.0.0.1:1235
LIMITER_THRESH=0
AUDIO_CHANNEL=0
PTT_TYPE=NONE
TIMEOUT=3600
TX_DELAY=0
```

What each part does:

- **`[SimplexLogic]`** — use whatever your actual logic section is called
  (`RepeaterLogic`, etc.); just point its `TX=` at the new `MultiTx`
  instead of your real transmitter directly.
- **`[MultiTx]`** — a `TYPE=Multi` transmitter fans audio out to several
  real/virtual transmitters at once. `TRANSMITTERS=Tx1,TxUDP` keeps your
  existing transmitter (`Tx1` — replace with its actual section name)
  working exactly as before, and adds the new virtual one alongside it, so
  RF transmission is unaffected.
- **`[TxUDP]`** — the virtual transmitter that feeds OpusRelay:
  - `AUDIO_DEV=udp:127.0.0.1:1235` — svxlink's native UDP audio output,
    pointed at the same host/port as `-pcmport` (default `1235`).
  - `LIMITER_THRESH=0` — disables the audio limiter for this output, so
    OpusRelay gets unprocessed audio rather than a compressed/limited
    signal meant for RF.
  - `PTT_TYPE=NONE` — no PTT hardware to key; this transmitter is
    UDP-only.
  - `TIMEOUT=3600` — safety cutoff (1 hour) in case something keeps this
    virtual transmitter keyed indefinitely.
  - `TX_DELAY=0` — no artificial delay before audio starts.

After editing `svxlink.conf`, restart the service:

```bash
systemctl restart svxlink
```

Then confirm audio is arriving by checking OpusRelay's own log for
`Audio gap` messages disappearing while svxlink is transmitting, or by
connecting with `example.html` and listening.

---

## Project layout

```
opusrelay/
├── webproxy.go       — the server (build target: go build webproxy.go)
├── token_gen.go       — standalone JWT generator for local testing
├── opus-player.js     — embeddable client library
├── example.html       — standalone demo/test client (no host page needed)
├── webproxy.service    — systemd unit
├── go.mod
├── LICENSE
└── .gitignore
```

`webproxy.go` and `token_gen.go` both declare `package main` with their own
`func main()`. That's intentional, but it means this directory is **not**
a normal buildable Go package — always build each file explicitly
(`go build webproxy.go`, `go run token_gen.go`), never `go build .` or
`go build ./...`, which would fail with a duplicate `main` error.

## 1. Installing a modern Go toolchain (Go 1.21)

```bash
# Download Go 1.21 directly from golang.org
cd /tmp
wget https://go.dev/dl/go1.21.13.linux-amd64.tar.gz
# (for ARM: go1.21.13.linux-arm64.tar.gz or linux-armv6l.tar.gz)

tar -C /usr/local -xzf go1.21.13.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin
echo 'export PATH=$PATH:/usr/local/go/bin' >> /root/.bashrc

go version   # → go version go1.21.13 linux/amd64
```

## 2. System dependencies

```bash
apt install libopus-dev gcc
```

> libopusfile is **not** needed. The code links against `libopus` directly.

## 3. Build

```bash
mkdir -p /opt/opusrelay
cp webproxy.go go.mod /opt/opusrelay/
cd /opt/opusrelay

# No "go mod tidy" needed — zero external dependencies
go build -o /usr/local/bin/webproxy webproxy.go
```

Typical build: ~5 seconds, ~4MB binary.

Optional smaller binary for production (strips debug symbols/DWARF info —
keep an unstripped build around if you ever need a readable stack trace):

```bash
go build -ldflags="-s -w" -o /usr/local/bin/webproxy webproxy.go
```

Note this is dynamically linked against `libopus` — the target machine
needs the `libopus0` runtime package installed (not just `libopus-dev`,
which is only needed at build time).

## 4. Installing the service

The service runs as a dedicated `webproxy` user, not root:

```bash
useradd --system --no-create-home --shell /usr/sbin/nologin webproxy
mkdir -p /var/log/webproxy
chown webproxy:webproxy /var/log/webproxy

# The JWT secret must only be readable by the service user
chown root:webproxy /opt/jwt.secret
chmod 640 /opt/jwt.secret

# Let's Encrypt certs need to be readable by the service user
# (certbot's default group is usually permissive enough, but check with
#  `getfacl` / `ls -la /etc/letsencrypt/live/...` if needed)

cp webproxy.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now webproxy
journalctl -u webproxy -f
```

## 5. Available flags

CLI flags override values loaded from `-config` (if used), which in turn
override the defaults below.

| Flag         | Default             | Description                            |
|--------------|---------------------|-----------------------------------------|
| -config      | -                   | Path to a JSON config file             |
| -gen-config  | *(required value)*  | Generate a config template file and exit — **takes the output path as its value**, e.g. `-gen-config config.json`. Running it bare (`-gen-config` with no path) is an error: `flag needs an argument: -gen-config` |
| -wsport      | 8080                | WebSocket port (WSS or WS)             |
| -pcmport     | 1235                | UDP port for the PCM audio source      |
| -udpip       | 127.0.0.1           | IP address the UDP listener binds to   |
| -cert        | (empty)             | TLS certificate path                   |
| -key         | (empty)             | TLS private key path                   |
| -bitrate     | 16000               | Opus bitrate in bps                    |
| -channels    | 1                   | Audio channels: 1 (mono) or 2 (stereo) |
| -mode        | speech              | Encoder profile: `speech` (narrowband-optimized, VOIP application) or `music` (full 20kHz bandwidth, higher complexity) — see below |
| -maxclients  | 500                 | Max simultaneous WS clients (0 = unlimited) |
| -log         | `/var/log/opus_relay.log` (Linux/macOS), `%TEMP%\opus_relay.log` (Windows) | Log file path — platform-aware default, see the Windows note below |
| -jwtsecret   | `/opt/jwt.secret` (Linux/macOS), `%TEMP%\jwt.secret` (Windows) | Path to the JWT secret file |
| -udpwaitwarn | 10                  | Seconds to wait for the first UDP audio packet before logging a warning (0 disables it) — see the logging note below |
| -testtone    | false               | Generate a 440Hz test tone instead of reading UDP |
| -debugjitter | false               | Log UDP gap diagnostics                |
| -notls       | false               | Disable TLS (for use behind a reverse proxy) |
| -noauth      | false               | Disable JWT authentication — local testing only, see below |
| -v           | -                   | Print the version and exit             |

> ⚠️ **`-noauth` fully disables authentication.** By default (no flag)
> the server always requires a valid JWT — the secure behaviour.
> `-noauth` is a plain boolean flag: writing it bare (`-noauth`) is
> exactly equivalent to `-noauth=true` — this is standard Go `flag`
> package behaviour for booleans, not something specific to this app.
> `-noauth=false` (or omitting it) keeps auth on. Use it only locally,
> to test quickly without generating tokens — never on a publicly
> reachable instance.

Generate an example config (the output path is a required argument, not
optional): `webproxy -gen-config config.json`

### Startup / connection logging

At startup the server logs that it's waiting for UDP
audio, and:

- If no packet arrives within `-udpwaitwarn` seconds (default 10), it
  logs a `WARNING: no audio received on ... after Ns` line pointing at
  the UDP address it's listening on — a clear signal to go check svxlink
  / ffmpeg / whatever is supposed to be sending.
- The first packet ever received logs `First audio packet received from
  ... — source is live`.
- Every start/stop of the audio (based on the same silence-gap detection
  used for the `talker_start`/`talker_stop` WS messages) now logs
  `Talker START (source: ...)` / `Talker STOP (silence detected, source:
  ...)` — previously these were only sent to WS clients, not logged.


### Measuring latency to a listener

The server already pings every connected WS client every 30 seconds to
keep idle connections alive; it now also times the round-trip to each
pong and reports it two ways:

- **Log**: `Latency to <addr>: <N>ms` every ~30s per connected client.
- **WS message**: a control frame `{"type":"latency","rtt_ms":<N>}` sent
  back to that same client, so a browser client can display it if it
  chooses to listen for that message type (neither `opus-player.js` nor
  `example.html` currently show it in the UI — this is just wired up on
  the wire for now).

This measures server↔client WebSocket round-trip time, not the total
audio pipeline latency (UDP source → encode → WS → decode → playback) —
those two are related but not the same number.

## 6. Reverse proxy mode (Nginx in front)

```bash
/usr/local/bin/webproxy -notls -wsport 8080 -pcmport 1235
```

```nginx
location /ws {
    proxy_pass         http://127.0.0.1:8080;
    proxy_http_version 1.1;
    proxy_set_header   Upgrade    $http_upgrade;
    proxy_set_header   Connection "upgrade";
    proxy_read_timeout 86400;
}
```

## 7. Browser clients

There are two, for two different use cases:

### `opus-player.js` — embeddable library

Drop this into an existing page (e.g. an svxlink status page) that already
has a `#togglePlayer` button and a `#peak-meter` container, plus jQuery
loaded. Configure it via globals set **before** the script tag:

```html
<script>
  window.AUDIO_SOCKET_URL = "wss://your-host:8080/"; // defaults to wss://example.net:8080/ if unset
  window.AUDIO_TOKEN      = "eyJhbGciOi...";
  window.AUDIO_CHANNELS   = 2; // defaults to 1 (mono) if unset — must match the server's -channels flag
</script>
<script src="opus-player.js"></script>
```

### `example.html` — standalone demo/test client

A self-contained page with its own UI — WebSocket URL and token as editable
input fields, a Channels dropdown (Mono/Stereo — must match the server's
`-channels` flag), connection status indicator, listener count, reset
button. Doesn't need a host page or any globals; just open it in a
browser, paste a URL and a token (or leave the token empty if the server
is running with `-noauth=true`), pick the channel count, and hit Start.
Useful both as a quick manual test tool and as a reference implementation.

### Protocol notes

Every binary message from the server has a 12-byte header
(`seq` uint32 LE + `timestamp` uint64 LE) followed by the Opus packet — the
client must strip those 12 bytes before handing the data to the decoder
(both clients already do this).

Safari doesn't have working `AudioDecoder`/Opus support via WebCodecs as of
this README — on Safari both clients show an error instead of starting
playback. Safari support would need a separate fallback (e.g. a WASM-based
`opus-stream-decoder`), which isn't included here.

---

## 8. Testing without a JWT (`-noauth`)

By default, the server always requires a valid JWT. If you just want to
quickly test the WS/Opus connection without generating a token every time,
start the server explicitly with `-noauth=true`:

```bash
webproxy -wsport 8080 -pcmport 1235 -notls -noauth=true
```

The server prints a visible warning in the log at startup for as long as
authentication is disabled. For testing with real authentication, or for
any public deployment, don't set this flag and use a token from
`token_gen.go` (next section).

## 9. Generating a JWT for testing

`token_gen.go` creates a token compatible with `validateJWT` in
`webproxy.go` (HS256, signed with the same secret as the server). This is
for local testing only — your real auth backend should issue tokens for
actual users.

```bash
# if you don't already have a secret at /opt/jwt.secret:
openssl rand -hex 32 > /opt/jwt.secret

go run token_gen.go \
    -email test@example.com \
    -level listener \
    -ttl 1h
```

The tool prints the token to stdout and a ready-to-use URL
(`wss://host:port/?token=...`) to stderr. Use `-secret-file` to point at a
different file than the default `/opt/jwt.secret`. Paste the printed token
straight into `example.html`'s token field for a quick manual test.

## Resource comparison

| Metric              | Node.js (raw WAV)  | Go (Opus)         |
|---------------------|--------------------|-------------------|
| Idle RAM             | ~60 MB             | ~6 MB             |
| RAM @ 50 clients     | ~120 MB            | ~18 MB            |
| Bandwidth/client     | ~768 kbps          | ~16–32 kbps       |
| External dependencies | ws, npm           | **zero** (stdlib) |
| Minimum Go version   | —                  | 1.13+             |

