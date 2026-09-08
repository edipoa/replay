# Design — live camera view (`aovivo.vianasociety.com.br`)

Status: accepted 2026-08-31. Implemented in the same change.
Revised 2026-09-06: public clip button removed (see decision 6b); uplink
re-measured on wired ethernet; scale path is now a Cloudflare Cache Rule, not R2
(see decisions 7b, 8).
Revised 2026-09-07: panorama polish (see "Two-camera panorama" below) — gold
centre seam removed, fullscreen button added, seam alignment made tunable via
`?`-knobs with a `?cal=1` on-page calibration panel.

## Understanding summary

- **What:** a public web page at `aovivo.vianasociety.com.br` showing both venue
  cameras live in the browser, near-live (HLS, ~5–20 s delay).
- **Why:** now — remote monitoring of the game by the operator / venue owner;
  later — a product feature for end users to watch the game.
- **Who:** now, 1–2 internal viewers; later, many end users (phase 2).
- **Key constraints:**
  - RTSP cameras (`192.168.0.105`, Dahua/Intelbras URL scheme) are only
    reachable from the deploy laptop.
  - Deploy box is a weak 2011 Acer laptop: Pentium B960 (**2 cores / 2
    threads**), 4 GB RAM, 5400 rpm HDD, **on flaky 2.4 GHz WiFi**. See
    `~/.claude/.../memory/deploy_server_specs.md`.
  - Venue uplink ≈ **1.3 Mbit/s**, unstable, already shared with clip uploads
    to R2.
- **Non-goals (now):** sub-2s latency, WebRTC, authentication, DVR/seek,
  serving tens of simultaneous viewers, changing the clip pipeline.

## The bandwidth reality

| Source | Bitrate |
| --- | --- |
| Main stream (`subtype=0`, 1080p) | ~2–4 Mbit/s per camera |
| Both main streams | ~4–8 Mbit/s |
| **Venue uplink** | **~1.3 Mbit/s** |
| Substream (`subtype=1`, ~640×480) | ~0.5 Mbit/s per camera → ~1 Mbit/s both |

The main stream does not fit the uplink — not directly, not via R2 (R2 still
carries one copy out of the venue). Only the substream fits.

## Decision: substream for live, main stream for clips

- Live view ingests `subtype=1`. The clip pipeline is **untouched** and keeps
  using `subtype=0` at full quality — quality matters where it ships (the clip
  is the product), not in the monitoring view.
- Live runs as a **separate `ffmpeg -c copy` process per camera** (not a second
  output on the ingestion ffmpeg):
  - it needs a different input URL (the substream) anyway;
  - it isolates the money-path (clip capture) from live-view failures;
  - `-c copy` is I/O-bound, so 2 extra persistent processes fit in 2 threads.
    During a replay-encode burst the live processes get starved and viewers
    rebuffer for ~30 s — acceptable, replays are infrequent.
  - cost: +1 RTSP session per camera (Intelbras/Dahua allow ~10–20).

## Architecture

```
camera subtype=1  ──▶  ffmpeg -c copy -f hls   (video/live.go, 1 goroutine/cam)
                          └─▶ <LiveDir>/<cam>/index.m3u8 + seg_*.ts   (tmpfs)
                                 └─▶ :8088  /live/<cam>/*             (internal/obs/live.go)
                                        └─▶ Host aovivo.*  →  hls.js page, one <video>/cam
                                               └─▶ Cloudflare Tunnel (new ingress rule)
```

### Components

**`video/` — live ingestion**
- `video.Config` gains `LiveDir` and `LiveRTSPUrl`. Both set ⇒ `Engine.Start`
  launches `runLiveHLS(ctx)` alongside `runIngestion` / `runCleanup`.
- `runLiveHLS` mirrors `runIngestion`'s retry/backoff loop. FFmpeg:
  ```
  ffmpeg -loglevel warning -rtsp_transport tcp -fflags +genpts -i <substream> \
    -c copy -f hls -hls_time 2 -hls_list_size 6 \
    -hls_flags delete_segments+omit_endlist -strftime 1 \
    -hls_segment_filename <LiveDir>/<cam>/seg_%Y%m%d_%H%M%S.ts \
    <LiveDir>/<cam>/index.m3u8
  ```
- Segment filenames reuse the `seg_<strftime>.ts` convention so the existing
  `video.NewestSegmentTime` / stall watchdog work unchanged. These cameras are
  known to wedge FFmpeg silently (see `ingestion_stall_fix` memory), so the
  live process reuses `watchForStall`.
- Retention: FFmpeg `delete_segments` — no Go cleanup ticker for the live dir.

**`internal/obs/live.go` — serving (new)**
- `GET /live/<cam>/index.m3u8` + `/live/<cam>/*.ts` served from `cfg.LiveDir`
  via `http.FileServer(http.Dir(...))`. Headers: `.m3u8` → `Cache-Control:
  no-cache`; `.ts` → `public, max-age=10`; `Access-Control-Allow-Origin: *`.
  404 when `LiveDir == ""`.
- `handleIndex`: `Host` starts with `aovivo.` ⇒ serve `liveHTML`. Same pattern
  as the existing `botao.` host branch.
- `liveHTML`: static dark-theme page. Camera list = sorted subdirectories of
  `LiveDir` that contain an `index.m3u8` (self-healing; no extra plumbing). One
  `<video controls autoplay muted playsinline>` per camera, responsive grid.
  hls.js from `cdn.jsdelivr.net` (fetched by the viewer's browser, not the
  venue); Safari/iOS uses native HLS. Playlist base URL is `/live` now, swap
  to a CDN origin in phase 2 with no page-logic change.

**`main.go` — config**
- `REPLAY_LIVE_ENABLED` (default `false`), `REPLAY_LIVE_DIR` (default
  `/tmp/replay_live`).
- Per-camera live URL: derived from `REPLAY_CAM_<n>_RTSP_URL` by replacing
  `subtype=0` → `subtype=1`; `REPLAY_CAM_<n>_LIVE_RTSP_URL` overrides. If
  neither yields a substream URL, live is skipped for that camera with a warn.
- `LiveDir` is also passed into `obs.Config`.

### Two-camera panorama (revised 2026-09-07)

With exactly two cameras `liveHTML` renders one panoramic strip (cam left, cam
right) instead of the grid. The two feeds butt at the centre; each keeps its
full width, letterboxed top/bottom. A single play gate drives both; a JS loop
keeps them on the same wall-clock instant via `EXT-X-PROGRAM-DATE-TIME`.

- **No centre divider.** An earlier build drew a gold "light-leak" line down
  the seam; removed — the brief was "make it look like one image".
- **Fullscreen button** in the header: adds `body.immersive` (CSS overlay
  hiding header/footer — works everywhere incl. iOS) *and* calls the
  Fullscreen API + `screen.orientation.lock('landscape')` where supported.
- **Seam alignment is hand-tuned, live, via `?`-knobs**, then baked into the
  `:root` custom properties (`--seaml/--seamr` inner-edge trim %, `--dxl/--dxr`
  `--dyl/--dyr` shift px, `--rotl/--rotr` roll deg, `--blend` cross-fade px).
  `?cal=1` shows an on-page panel: sliders for trim/blend, drag a feed to move
  it, shift-drag to roll it, "ver cru" to zero everything, and a live `?query`
  readout to copy. Current bake: `--seaml:19%; --seamr:18.5%`, everything else
  0 — a plain symmetric-ish clip read best; shift/roll/blend all landed at 0.
  Two cameras with no overlap can't stitch perfectly for near objects; the
  knobs get it close and `--blend` (if set) softens the rest.

### Ops (run on the deploy laptop)

```sh
# tmpfs for the rolling live segments (HDD can't take the churn)
echo 'tmpfs /tmp/replay_live tmpfs noatime,size=128M,mode=1777 0 0' | sudo tee -a /etc/fstab
sudo mkdir -p /tmp/replay_live && sudo mount -a

# .env
REPLAY_LIVE_ENABLED=true
```

Cloudflare Tunnel — add to `~/.cloudflared/config.yml` ingress, **before** the
catch-all `- service: http_status:404`:

```yaml
  - hostname: aovivo.vianasociety.com.br
    service: http://localhost:8088
```

```sh
cloudflared tunnel route dns <tunnel-name> aovivo.vianasociety.com.br
sudo systemctl restart cloudflared
```

## Decision log

| # | Decision | Alternatives | Why |
| --- | --- | --- | --- |
| 1 | Substream for live, main stream for clips | main for both; re-encode live | 1.3 Mbit/s uplink can't carry the main stream; re-encode doesn't fit 2 threads |
| 2 | Separate `ffmpeg -c copy` per camera for live | 2nd output on the ingestion ffmpeg | different input URL required; isolates clip capture from live failures |
| 3 | Live segments on tmpfs | on the HDD | 5400 rpm HDD can't take the rotation churn alongside ingestion + encode |
| 4 | `seg_<strftime>.ts` segment names for live | sequential HLS names | reuses `NewestSegmentTime` + stall watchdog unchanged |
| 5 | hls.js via jsdelivr CDN | embed in the binary | the viewer's browser fetches it, not the venue; keeps 300 KB out of the binary |
| 6 | No auth on the page | PIN like `/botao` | operator's call; page exposes only the substream |
| 6b | ~~Public clip button (`POST /clip`)~~ **reverted 2026-09-06** — page is view-only again | keep it; just raise the cooldown; rate-limit per IP | owner's call. With the wired uplink the target is a wide public audience, and a single 60 s global cooldown can't arbitrate a clip button shared by hundreds of viewers. The operator loses nothing — physical button, joystick and `/botao` → `/trigger` are unchanged. `handleLiveClip`, the `/clip` route, the `liveClip*` consts and the page's clip UI were all removed. |
| 7 | R2 push deferred to phase 2 | do it now | direct-from-laptop was capped by the 1.3 Mbit/s uplink |
| 7b | R2 push **superseded** by a Cloudflare Cache Rule (phase 1.5) | build the R2 sync now | wired uplink (~56 Mbit/s) + edge-cached `.ts`/`.m3u8` covers hundreds of viewers with **no code and no cost**; R2 only becomes worth it past that, or if Cloudflare flags the video volume under ToS 2.8 |
| 8 | Cache `.m3u8` for 1 s via a Cloudflare Edge-TTL override, not a Go header change | set `Cache-Control: max-age=1` in `liveHandler` | the dashboard override wins over `no-cache`; keeps the origin header correct for any direct-from-origin use and keeps the change out of the binary |

## Capacity (phase 1 + 1.5)

Direct-from-laptop (phase 1, no edge cache): each viewer pulls both substreams
≈ **1 Mbit/s**; against the wired uplink (~56 Mbit/s, re-measured 2026-09-05,
was 1.3 Mbit/s on WiFi) and leaving headroom for clip uploads that is
**~15–25 simultaneous viewers**. The old "~1 viewer" figure was the WiFi era.

Phase 1.5 — **Cloudflare Cache Rule** on `aovivo.vianasociety.com.br/live/*`:

- `*.ts` → *Eligible for cache*, Edge TTL **override 60 s** (segment names are
  timestamped and never rewritten — immutable).
- `*.m3u8` → *Eligible for cache*, Edge TTL **override 1 s** (overrides the
  origin `no-cache`; 1 s stale playlist is nothing against ~15–20 s HLS
  latency).

Origin load then flattens to **~5 Mbit/s** (one pull per Cloudflare PoP per
segment, ~3–5 PoPs in Brazil) regardless of whether 50 or 500 people are
watching. `aovivo.*` is already proxied through the edge (created by
`cloudflared tunnel route dns`), so Cache Rules apply to tunnel traffic. No code
change.

**ToS caveat:** a few hundred substream viewers ≈ 100–300 Mbit/s of video out of
Cloudflare's cache on a Free plan can draw a ToS 2.8 enforcement email (a
request to move to Cloudflare Stream or R2 — not a bill). If that happens, phase
2 (R2, free egress) is the sanctioned path.

Replay generation still stays on the one field laptop (one ffmpeg, press-queue
cap 1) — unchanged by any of this.

## Phase 2 sketch (not implemented)

- A goroutine watches `LiveDir`, uploads new `.ts` + rewrites `index.m3u8` to
  R2 on every segment, reusing `upload/`'s retry client.
- `REPLAY_LIVE_BASE_URL` config; when set, `liveHTML` points `<video>` at
  `<base>/<cam>/index.m3u8` instead of `/live/<cam>/index.m3u8`.
- Everything else (ingestion, page, routing) is unchanged.
