package obs

import (
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
)

// liveHandler serves the rolling HLS files written by the video package under
// <LiveDir>/<cam>/ (index.m3u8 + seg_*.ts) at /live/<cam>/... . Static files
// only — no directory listing.
func (r *Recorder) liveHandler() http.Handler {
	fs := http.StripPrefix("/live/", http.FileServer(http.Dir(r.cfg.LiveDir)))
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/") {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		switch path.Ext(req.URL.Path) {
		case ".m3u8":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Cache-Control", "no-cache")
		case ".ts":
			w.Header().Set("Content-Type", "video/mp2t")
			w.Header().Set("Cache-Control", "public, max-age=10")
		}
		fs.ServeHTTP(w, req)
	})
}

// liveCameras returns the camera ids that currently have a live playlist,
// sorted. Derived from the filesystem so a camera shows up as soon as its
// FFmpeg produces a playlist and disappears if live is reconfigured.
func (r *Recorder) liveCameras() []string {
	entries, err := os.ReadDir(r.cfg.LiveDir)
	if err != nil {
		return nil
	}
	var cams []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(path.Join(r.cfg.LiveDir, e.Name(), "index.m3u8")); err == nil {
			cams = append(cams, e.Name())
		}
	}
	sort.Strings(cams)
	return cams
}

func (r *Recorder) handleLivePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := liveTmpl.Execute(w, r.liveCameras()); err != nil {
		slog.Warn("obs: render live page failed", slog.Any("error", err))
	}
}

// liveTmpl is the whole aovivo.* page. hls.js and the display font are fetched
// from public CDNs by the viewer's browser (never the venue uplink).
//
// With exactly two cameras it renders a single panoramic strip — cam left,
// cam right, no gap. Each half keeps its FULL width (no horizontal crop, so
// the midfield each camera sees is never lost); the strip is as tall as that
// makes it, letterboxed top/bottom against black. A JS loop keeps both
// <video> elements on the same wall-clock instant using the
// EXT-X-PROGRAM-DATE-TIME tags FFmpeg writes (see video/live.go). Any other
// camera count falls back to a plain responsive grid with no sync.
//
// URL knobs (bookmark the tuned link, no redeploy): ?seam=<n> trims n% off
// each inner edge to hide the midfield overlap (default 4; 0 = nothing
// trimmed, full field both sides with a visible join); ?swap=1 flips
// left/right; ?sync=off disables the aligner; ?selftest=1 asserts the drift
// math in the console.
//
// Aesthetic: "broadcast truck" — GitHub-dark base, one signal-green accent,
// a condensed Bebas Neue wordmark, a pulsing tally light, mono uppercase
// camera slugs. The memorable anchor is the seam: a thin green light-leak
// down the exact centre where the two feeds meet.
var liveTmpl = template.Must(template.New("live").Parse(`<!doctype html>
<html lang="pt-br"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>Ao vivo — Viana Society</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Bebas+Neue&display=swap">
<style>
 :root{--bg:#0d1117;--fg:#e6edf3;--dim:#8b949e;--line:#30363d;--live:#3fb950;--seam:4%}
 *{box-sizing:border-box}
 html,body{height:100%}
 body{margin:0;background:var(--bg);color:var(--fg);
  font:15px/1.5 ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,sans-serif}
 header{display:flex;align-items:center;gap:.6rem;height:2.9rem;padding:0 1rem;
  border-bottom:1px solid var(--line)}
 .wordmark{font-family:"Bebas Neue",Impact,sans-serif;font-size:1.4rem;
  letter-spacing:.13em;line-height:1}
 .tally{width:.5rem;height:.5rem;border-radius:50%;background:var(--live);
  animation:tally 2s infinite}
 .tally-label{font-family:"Bebas Neue",Impact,sans-serif;letter-spacing:.2em;
  font-size:.85rem;color:var(--live)}
 @keyframes tally{
  0%{box-shadow:0 0 0 0 rgba(63,185,80,.7)}
  70%{box-shadow:0 0 0 .55rem rgba(63,185,80,0)}
  100%{box-shadow:0 0 0 0 rgba(63,185,80,0)}}
 @media (prefers-reduced-motion:reduce){.tally{animation:none}}

 /* panorama strip — exactly two cameras */
 .strip{display:flex;width:100%;min-height:calc(100dvh - 2.9rem);
  align-items:center;background:#000}
 .strip figure{position:relative;flex:1 1 0;min-width:0;margin:0;
  overflow:hidden;font-size:0}
 .strip video{width:100%;height:auto;display:block;background:#000}
 @media (orientation:landscape){
  /* full width kept; --seam only clips a sliver at the centre join to hide
     the midfield overlap. The video is drawn (100% + seam) wide inside an
     overflow:hidden figure, so exactly seam is cut from the inner edge. */
  .strip figure video{width:calc(100% + var(--seam))}
  .strip figure[data-side=right] video{margin-left:calc(-1 * var(--seam))}
  .strip figure[data-side=left]::after{content:"";position:absolute;inset:0 0 0 auto;
   width:2px;pointer-events:none;
   background:linear-gradient(rgba(63,185,80,0),var(--live) 55%,rgba(63,185,80,0))}
 }
 @media (orientation:portrait){
  .strip{flex-direction:column;align-items:stretch;min-height:0}
 }
 figcaption{position:absolute;left:.5rem;top:.5rem;padding:.16rem .5rem;
  font:600 .72rem/1.4 ui-monospace,SFMono-Regular,Menlo,monospace;
  letter-spacing:.06em;text-transform:uppercase;
  background:rgba(0,0,0,.6);border:1px solid var(--line);border-radius:3px}

 /* fallback grid — zero / one / 3+ cameras */
 .grid{display:grid;gap:2px;padding:2px;
  grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}
 .grid figure{margin:0;position:relative;background:#000}
 .grid video{width:100%;display:block;aspect-ratio:16/9;background:#000}
 .empty{padding:2rem 1rem;color:var(--dim)}
</style></head><body>
<header>
 <span class="wordmark">Viana Society</span>
 <span class="tally" aria-hidden="true"></span>
 <span class="tally-label">Ao vivo</span>
</header>
{{if .}}
{{if eq (len .) 2}}
<div class="strip">
{{range .}} <figure id="f-{{.}}"><video id="v-{{.}}" autoplay muted playsinline></video><figcaption>{{.}}</figcaption></figure>
{{end}}</div>
{{else}}
<div class="grid">
{{range .}} <figure><video id="v-{{.}}" controls autoplay muted playsinline></video><figcaption>{{.}}</figcaption></figure>
{{end}}</div>
{{end}}
<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.17/dist/hls.min.js"></script>
<script>
var cams = [{{range $i, $c := .}}{{if $i}},{{end}}"{{$c}}"{{end}}];
var qp = new URLSearchParams(location.search);

var seam = qp.get('seam');
if (seam !== null) {
  var sn = parseFloat(seam);
  document.documentElement.style.setProperty('--seam',
    isNaN(sn) ? seam : Math.max(0, sn) + '%');
}

if (qp.get('swap') === '1') cams.reverse();

// left/right framing follows visual order (strip layout only)
cams.forEach(function(cam, i){
  var fig = document.getElementById('f-'+cam);
  if (!fig) return;
  fig.style.order = i;
  fig.dataset.side = i === 0 ? 'left' : 'right';
});

var players = cams.map(function(cam){
  var v = document.getElementById('v-'+cam);
  var src = '/live/'+cam+'/index.m3u8';
  var hls = null;
  if (window.Hls && Hls.isSupported()) {
    hls = new Hls({liveSyncDurationCount:4, liveMaxLatencyDurationCount:15, backBufferLength:30});
    hls.loadSource(src);
    hls.attachMedia(v);
    hls.on(Hls.Events.ERROR, function(_e, data){
      if (data.fatal) setTimeout(function(){ try{ hls.loadSource(src); hls.startLoad(); }catch(e){} }, 3000);
    });
  } else {
    v.src = src; // Safari / iOS native HLS
  }
  return {v:v, hls:hls};
});

// wall-clock ms at a player's playhead, from EXT-X-PROGRAM-DATE-TIME
function wallMs(p){
  try { if (p.hls && p.hls.playingDate) return p.hls.playingDate.getTime(); } catch(e){}
  try {
    var s = p.v.getStartDate && p.v.getStartDate();
    if (s && !isNaN(s.getTime())) return s.getTime() + p.v.currentTime * 1000;
  } catch(e){}
  return NaN;
}

// pure: seconds this player runs AHEAD of the master -> what to do about it
function correction(delta){
  if (delta > 2)    return {seek:-delta, rate:1};    // too far: jump back
  if (delta > 0.15) return {seek:0,      rate:0.97}; // drifting: ease back
  return {seek:0, rate:1};                           // aligned
}

function sync(){
  var t = players.map(wallMs);
  if (t.some(isNaN)) return;
  var master = Math.min.apply(null, t);
  players.forEach(function(p, i){
    var c = correction((t[i] - master) / 1000);
    if (c.seek && p.v.buffered.length &&
        p.v.currentTime + c.seek > p.v.buffered.start(0)) {
      p.v.currentTime += c.seek;
    }
    if (p.v.playbackRate !== c.rate) p.v.playbackRate = c.rate;
  });
}
if (qp.get('sync') !== 'off' && players.length === 2) setInterval(sync, 2000);

if (qp.get('selftest') === '1') {
  console.assert(correction(5).seek === -5 && correction(5).rate === 1, 'big drift -> seek back');
  console.assert(correction(1).seek === 0 && correction(1).rate === 0.97, 'small drift -> ease');
  console.assert(correction(0.05).seek === 0 && correction(0.05).rate === 1, 'aligned -> rate 1');
  console.log('selftest ok');
}
</script>
{{else}}
<p class="empty">Nenhuma câmera ao vivo no momento.</p>
{{end}}
</body></html>`))
