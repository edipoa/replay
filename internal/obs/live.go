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
// makes it, letterboxed top/bottom against a navy field. A JS loop keeps both
// <video> elements on the same wall-clock instant using the
// EXT-X-PROGRAM-DATE-TIME tags FFmpeg writes (see video/live.go). A single
// play gate covers the strip (the per-video native play button is hidden):
// clicking it plays both feeds, pausing either one pauses both. Any other
// camera count falls back to a plain responsive grid with no sync.
//
// Seam calibration — the two cameras see the midfield from different angles,
// so the join is tuned by hand, live, via URL knobs (bookmark the tuned link,
// then bake the values as the :root defaults, no redeploy needed to try one):
//   ?seam=<n>    trim n% off BOTH inner edges (shorthand for seaml+seamr)
//   ?seaml=<n>   trim n% off the left feed's inner (right) edge
//   ?seamr=<n>   trim n% off the right feed's inner (left) edge
//   ?dxl/?dxr=<px>   shift a feed horizontally (negative = left)
//   ?dyl/?dyr=<px>   shift a feed vertically (negative = up)
//   ?rotl/?rotr=<deg>  roll a feed (the cameras are not perfectly level)
//   ?blend=<px>  cross-fade width at the join — the feeds overlap by <px> and
//                the right one's edge fades in, so there is no hard cut
// ?cal=1 opens calibration mode: sliders for trim + blend, DRAG a feed to
// move it, SHIFT-drag to roll it, "ver cru" to zero everything and see the raw
// feeds, and a live ?query readout — copy it, paste the values into :root.
// Other knobs: ?swap=1 flips left/right; ?sync=off disables the wall-clock
// aligner; ?selftest=1 asserts the drift math in the console.
//
// Aesthetic: same brand as the clips site (replay-site) — "stadium
// broadcast": deep-navy field, gold accent, Archivo Black wordmark,
// JetBrains Mono camera slugs, a broadcast-red pulsing live dot. Colour /
// type tokens mirror replay-site/frontend/src/style.css so aovivo.* and the
// clips site read as one product.
//
// A fullscreen button in the header drops the page into an immersive view
// (video + the sponsor bar; header and footer hidden) and, where the browser
// allows it, enters real fullscreen and locks to landscape; iOS Safari gets
// the CSS overlay alone.
var liveTmpl = template.Must(template.New("live").
	Funcs(template.FuncMap{"sponsors": func() []sponsor { return sponsors }}).
	Parse(`<!doctype html>
<html lang="pt-br"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>Ao vivo — Campo Society Viana</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=Archivo+Black&family=Archivo:wght@400;700&family=JetBrains+Mono:wght@400;700&display=swap">
<style>
 :root{
  --navy:#0E2A5E;--navy-deep:#07153A;--gold:#E8B842;--gold-deep:#C8961E;
  --paper:#F7F4ED;--muted:rgba(247,244,237,.6);--live:#FF3B30;
  --hdr:3.25rem;--sp-h:4.75rem;
  /* seam calibration — see the doc comment on liveTmpl. Bake tuned values here.
     Tuned live via ?cal=1 on 2026-09-07: plain symmetric-ish trim reads best;
     shift/roll/blend all landed back at 0. */
  --seaml:19%;--seamr:18.5%;--dxl:0px;--dxr:0px;--dyl:0px;--dyr:0px;
  --rotl:0deg;--rotr:0deg;--blend:0px}
 @media (max-height:520px){:root{--sp-h:3.5rem}}
 *{box-sizing:border-box}
 html,body{height:100%}
 body{margin:0;background:var(--navy-deep);color:var(--paper);
  font:15px/1.5 "Archivo",system-ui,-apple-system,Segoe UI,Roboto,sans-serif}

 /* header: navy bar under a 4px gold rule — the site topbar signature */
 header{display:flex;align-items:center;gap:.85rem;height:var(--hdr);
  padding:0 1rem;background:var(--navy);border-bottom:4px solid var(--gold)}
 .wordmark{font-family:"Archivo Black","Archivo",sans-serif;font-size:1.15rem;
  letter-spacing:.02em;line-height:1;text-transform:uppercase;white-space:nowrap}
 .wordmark .accent{color:var(--gold)}
 .sub{font-family:"JetBrains Mono",ui-monospace,monospace;font-size:.62rem;
  letter-spacing:.2em;text-transform:uppercase;color:var(--muted);
  padding-left:.85rem;border-left:1px solid rgba(247,244,237,.15)}
 @media (max-width:560px){
  .sub{display:none}
  .wordmark{font-size:1rem}
  .live-pill{padding:.42rem .7rem;letter-spacing:.1em}
 }
 .spacer{flex:1}
 .live-pill{display:inline-flex;align-items:center;gap:.5rem;
  background:rgba(255,255,255,.06);border:1px solid rgba(255,255,255,.1);
  padding:.5rem .85rem;border-radius:999px;
  font:700 .72rem/1 "JetBrains Mono",ui-monospace,monospace;
  letter-spacing:.14em;text-transform:uppercase;white-space:nowrap}
 .live-dot{width:.5rem;height:.5rem;border-radius:50%;background:var(--live);
  animation:pulse 1.6s infinite}
 @keyframes pulse{
  0%{box-shadow:0 0 0 0 rgba(255,59,48,.5)}
  70%{box-shadow:0 0 0 .75rem rgba(255,59,48,0)}
  100%{box-shadow:0 0 0 0 rgba(255,59,48,0)}}
 @media (prefers-reduced-motion:reduce){.live-dot{animation:none}}

 /* fullscreen toggle — immersive CSS overlay + native FS where supported */
 .fs-btn{display:inline-flex;align-items:center;justify-content:center;
  width:2.4rem;height:2.4rem;padding:0;cursor:pointer;color:var(--paper);
  background:rgba(255,255,255,.06);border:1px solid rgba(255,255,255,.1);
  border-radius:999px}
 .fs-btn svg{width:1.15rem;height:1.15rem}
 .fs-btn:focus-visible{outline:2px solid var(--gold);outline-offset:2px}
 .fs-btn .i-close{display:none}
 body.immersive header,body.immersive .ftr{display:none}
 body.immersive{overflow:hidden}
 body.immersive .strip{min-height:100dvh}
 body.immersive.has-sp .strip{min-height:calc(100dvh - var(--sp-h))}
 body.immersive .fs-btn{position:fixed;top:.6rem;right:.6rem;z-index:60;
  background:rgba(7,21,58,.7);backdrop-filter:blur(4px)}
 body.immersive .fs-btn .i-open{display:none}
 body.immersive .fs-btn .i-close{display:inline}

 /* ?cal=1 — live seam-calibration panel (hidden from normal viewers) */
 #cal{position:fixed;left:.6rem;bottom:.6rem;z-index:80;
  display:flex;flex-direction:column;gap:.3rem;padding:.7rem .8rem;border-radius:8px;
  background:rgba(7,21,58,.93);border:1px solid rgba(232,184,66,.45);
  font:600 .7rem/1.2 "JetBrains Mono",ui-monospace,monospace;
  color:var(--paper);max-width:min(92vw,340px)}
 #cal label{display:flex;align-items:center;gap:.55rem;white-space:nowrap}
 #cal input[type=range]{flex:1;min-width:0}
 #cal output{width:2.7rem;text-align:right;color:var(--gold)}
 #cal #cal-url{margin-top:.35rem;padding:.4rem .5rem;border-radius:4px;
  background:rgba(0,0,0,.4);color:var(--gold);word-break:break-all;user-select:all}
 #cal .cal-hint{color:var(--muted);font-size:.62rem;white-space:normal}
 #cal #cal-xy{color:var(--paper);opacity:.85;font-size:.66rem}
 #cal #cal-reset{align-self:flex-start;padding:.25rem .6rem;cursor:pointer;
  color:var(--paper);background:rgba(255,255,255,.08);
  border:1px solid rgba(255,255,255,.15);border-radius:4px;font:inherit}
 #cal-guideline{position:absolute;top:0;bottom:0;left:50%;width:1px;z-index:70;
  pointer-events:none;background:rgba(232,184,66,.8)}
 body.cal .strip figure{cursor:grab}
 body.cal .strip figure:active{cursor:grabbing}

 /* panorama strip — exactly two cameras */
 .strip{display:flex;width:100%;min-height:calc(100dvh - var(--hdr));
  align-items:center;position:relative;background:var(--navy-deep)}
 .has-sp .strip{min-height:calc(100dvh - var(--hdr) - var(--sp-h))}
 /* letterbox bars catch a faint gold glow + broadcast scanlines (same
    texture as the site hero); sits behind the video, shows top/bottom only */
 .strip::before{content:"";position:absolute;inset:0;z-index:0;pointer-events:none;
  background:
   radial-gradient(ellipse 70% 55% at 50% 50%,rgba(232,184,66,.10),transparent 62%),
   repeating-linear-gradient(90deg,transparent 0 58px,rgba(255,255,255,.02) 58px 59px)}
 .strip figure{position:relative;z-index:1;flex:1 1 0;min-width:0;margin:0;
  overflow:hidden;font-size:0}
 .strip video{width:100%;height:auto;display:block;background:var(--navy-deep)}
 @media (orientation:landscape){
  /* per-side seam calibration (all URL-tunable, see the liveTmpl doc comment).
     Each feed is drawn (100% + its seam) wide inside an overflow:hidden figure,
     so exactly that much is cut from the inner edge; --dx/--dy shift it and
     --rot rolls it. --blend overlaps the feeds and fades the right one's inner
     edge in over the left, so the join has no hard cut. */
  .strip figure[data-side=left] video{width:calc(100% + var(--seaml));
   transform:translate(var(--dxl),var(--dyl)) rotate(var(--rotl))}
  .strip figure[data-side=right] video{width:calc(100% + var(--seamr));
   margin-left:calc(-1 * var(--seamr));
   transform:translate(var(--dxr),var(--dyr)) rotate(var(--rotr))}
  .strip figure[data-side=right]{z-index:2;margin-left:calc(-1 * var(--blend));
   -webkit-mask-image:linear-gradient(90deg,transparent,#000 var(--blend));
   mask-image:linear-gradient(90deg,transparent,#000 var(--blend))}
 }
 @media (orientation:portrait){
  .strip{flex-direction:column;align-items:stretch;min-height:0}
 }

 /* rotate-your-phone gate — the panorama only works in landscape */
 .rotate{display:none}
 @media (orientation:portrait) and (max-width:820px){
  .rotate{position:fixed;inset:0;z-index:100;
   display:flex;flex-direction:column;align-items:center;justify-content:center;
   gap:1.1rem;padding:2rem;text-align:center;background:var(--navy-deep)}
  body{overflow:hidden}
 }
 .rotate svg{width:4.5rem;height:4.5rem;color:var(--gold)}
 .rotate svg .ph{transform-origin:12px 12px;animation:tip 2.6s ease-in-out infinite}
 @keyframes tip{0%,48%,100%{transform:rotate(0)}66%,90%{transform:rotate(90deg)}}
 @media (prefers-reduced-motion:reduce){.rotate svg .ph{animation:none}}
 .rotate-t{margin:0;font-family:"Archivo Black","Archivo",sans-serif;
  font-size:1.5rem;text-transform:uppercase;letter-spacing:.02em}
 .rotate-s{margin:0;max-width:22rem;color:var(--muted);
  font:.8rem/1.6 "JetBrains Mono",ui-monospace,monospace;letter-spacing:.04em}

 figcaption{position:absolute;left:.6rem;top:.6rem;z-index:2;padding:.22rem .5rem;
  font:700 .68rem/1.3 "JetBrains Mono",ui-monospace,monospace;
  letter-spacing:.08em;text-transform:uppercase;color:#fff;
  background:rgba(7,21,58,.85);border:1px solid rgba(232,184,66,.35);border-radius:3px}

 /* fallback grid — zero / one / 3+ cameras */
 .grid{display:grid;gap:2px;padding:2px;
  grid-template-columns:repeat(auto-fit,minmax(320px,1fr))}
 .grid figure{margin:0;position:relative;background:var(--navy-deep)}
 .grid video{width:100%;display:block;aspect-ratio:16/9;background:var(--navy-deep)}
 .empty{padding:3rem 1.5rem;color:var(--muted);text-align:center;
  font-family:"JetBrains Mono",ui-monospace,monospace;letter-spacing:.06em}

 /* play gate — a single control layered over the whole panorama; the native
    per-video play button is suppressed so both feeds move together */
 .playgate{position:absolute;inset:0;z-index:3;border:0;margin:0;padding:2rem;
  display:flex;flex-direction:column;align-items:center;justify-content:center;
  gap:1rem;cursor:pointer;color:var(--paper);
  background:radial-gradient(ellipse 60% 50% at 50% 50%,rgba(7,21,58,.35),rgba(7,21,58,.7))}
 .playgate[hidden]{display:none}
 .pg-disc{width:5rem;height:5rem;border-radius:50%;display:grid;place-items:center;
  background:var(--gold);color:var(--navy-deep);
  box-shadow:0 10px 40px rgba(0,0,0,.45),0 0 0 1px rgba(232,184,66,.5);
  transition:transform .15s,box-shadow .15s}
 .pg-disc svg{width:2.2rem;height:2.2rem;margin-left:.2rem}
 .playgate:hover .pg-disc{transform:scale(1.06);
  box-shadow:0 12px 48px rgba(0,0,0,.5),0 0 0 4px rgba(232,184,66,.3)}
 .playgate:focus-visible .pg-disc{box-shadow:0 0 0 4px var(--gold)}
 .pg-label{font:700 .78rem/1 "JetBrains Mono",ui-monospace,monospace;
  letter-spacing:.22em;text-transform:uppercase}
 .strip video::-webkit-media-controls-start-playback-button,
 .strip video::-webkit-media-controls-overlay-play-button{
  display:none!important;-webkit-appearance:none}

 /* sponsor bar — docked under the video, inside the first screen */
 .sponsors{display:flex;align-items:center;justify-content:center;
  gap:1.5rem;height:var(--sp-h);padding:0 1.25rem;background:var(--navy);
  border-top:1px solid rgba(232,184,66,.25)}
 .sp-label{flex-shrink:0;
  font:700 .58rem/1 "JetBrains Mono",ui-monospace,monospace;
  letter-spacing:.22em;text-transform:uppercase;color:var(--muted)}
 @media (max-width:560px){.sp-label{display:none}}
 .sp-logos{display:flex;align-items:center;gap:1.5rem;
  flex-wrap:wrap;justify-content:center;min-width:0}
 .sp{display:inline-flex;align-items:center;height:calc(var(--sp-h) - 2rem);
  padding:.35rem .7rem;border-radius:8px;
  background:rgba(255,255,255,.04);border:1px solid rgba(255,255,255,.08)}
 .sp.plate{background:#fff;border-color:transparent;padding:.4rem 1rem}
 .sp img{height:100%;width:auto;display:block}
 a.sp{transition:transform .15s,border-color .15s}
 a.sp:hover{transform:translateY(-2px);border-color:rgba(232,184,66,.4)}

 /* footer — the site sign-off */
 .ftr{background:var(--navy);color:rgba(247,244,237,.7);
  padding:2rem 1.5rem;text-align:center;
  font:12px/1.7 "JetBrains Mono",ui-monospace,monospace;letter-spacing:.08em}
 .ftr .gold{color:var(--gold)}
 .ftr-sub{opacity:.6;margin-top:.5rem}
</style></head><body class="{{if sponsors}}has-sp{{end}}">
<header>
 <span class="wordmark">CAMPO SOCIETY<span class="accent">·</span>VIANA</span>
 <span class="sub">Transmissão ao vivo</span>
 <span class="spacer"></span>
 <span class="live-pill"><span class="live-dot" aria-hidden="true"></span>Ao vivo</span>
{{if .}} <button class="fs-btn" type="button" aria-label="Tela cheia" aria-pressed="false">
  <svg class="i-open" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 3H5a2 2 0 0 0-2 2v3m18 0V5a2 2 0 0 0-2-2h-3m0 18h3a2 2 0 0 0 2-2v-3M8 21H5a2 2 0 0 1-2-2v-3"/></svg>
  <svg class="i-close" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M8 3v3a2 2 0 0 1-2 2H3m18 0h-3a2 2 0 0 1-2-2V3m0 18v-3a2 2 0 0 1 2-2h3M3 16h3a2 2 0 0 1 2 2v3"/></svg>
 </button>{{end}}
</header>
{{if .}}
{{if eq (len .) 2}}
<div class="rotate">
 <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
  <path d="M4.4 11.5a7.6 7.6 0 0 0 3.4 6.4"/>
  <path d="M3.5 7.7 4.4 11.7 8 10.4"/>
  <g class="ph"><rect x="8.6" y="2" width="6.8" height="20" rx="1.6"/><line x1="11" y1="5" x2="13" y2="5"/></g>
 </svg>
 <p class="rotate-t">Gire o celular</p>
 <p class="rotate-s">A transmissão é panorâmica — vire o aparelho para a horizontal.</p>
</div>
<div class="strip">
<button class="playgate" type="button" aria-label="Assistir ao vivo">
 <span class="pg-disc"><svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg></span>
 <span class="pg-label">Assistir</span>
</button>
{{range .}} <figure id="f-{{.}}"><video id="v-{{.}}" autoplay muted playsinline></video><figcaption>{{.}}</figcaption></figure>
{{end}}</div>
{{else}}
<div class="grid">
{{range .}} <figure><video id="v-{{.}}" controls autoplay muted playsinline></video><figcaption>{{.}}</figcaption></figure>
{{end}}</div>
{{end}}
{{if sponsors}}
<aside class="sponsors">
 <span class="sp-label">Patrocínio</span>
 <div class="sp-logos">
{{range sponsors}} {{if .URL}}<a class="sp{{if .Plate}} plate{{end}}" href="{{.URL}}" target="_blank" rel="noopener"><img src="{{.Img}}" alt="{{.Name}}" loading="lazy"></a>{{else}}<span class="sp{{if .Plate}} plate{{end}}"><img src="{{.Img}}" alt="{{.Name}}" loading="lazy"></span>{{end}}
{{end}} </div>
</aside>
{{end}}
<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.17/dist/hls.min.js"></script>
<script>
var cams = [{{range $i, $c := .}}{{if $i}},{{end}}"{{$c}}"{{end}}];
var qp = new URLSearchParams(location.search);

// seam calibration knobs — number => append unit, otherwise pass the raw
// string through (so ?blend=2rem still works). See the liveTmpl doc comment.
function setLen(prop, raw, unit, min) {
  if (raw === null) return;
  var n = parseFloat(raw);
  if (!isNaN(n) && min !== undefined) n = Math.max(min, n);
  document.documentElement.style.setProperty(prop, isNaN(n) ? raw : n + unit);
}
var seam = qp.get('seam');
setLen('--seaml', seam, '%', 0);
setLen('--seamr', seam, '%', 0);
setLen('--seaml', qp.get('seaml'), '%', 0);
setLen('--seamr', qp.get('seamr'), '%', 0);
setLen('--dxl', qp.get('dxl'), 'px');
setLen('--dxr', qp.get('dxr'), 'px');
setLen('--dyl', qp.get('dyl'), 'px');
setLen('--dyr', qp.get('dyr'), 'px');
setLen('--rotl', qp.get('rotl'), 'deg');
setLen('--rotr', qp.get('rotr'), 'deg');
setLen('--blend', qp.get('blend'), 'px', 0);

if (qp.get('swap') === '1') cams.reverse();

// left/right framing follows visual order (strip layout only)
cams.forEach(function(cam, i){
  var fig = document.getElementById('f-'+cam);
  if (!fig) return;
  fig.style.order = i;
  fig.dataset.side = i === 0 ? 'left' : 'right';
});

// ?cal=1 — calibration mode. Sliders for trim + blend; DRAG a feed to move it,
// SHIFT-drag to roll it. It prints the full ?query live — copy it and bake the
// values into :root. Hidden unless the URL asks for it, so normal viewers
// never see it.
if (qp.get('cal') === '1') {
  document.body.classList.add('cal');
  var pg = document.querySelector('.playgate'); if (pg) pg.hidden = true; // don't block drags
  var num = function(p){ return parseFloat(getComputedStyle(document.documentElement).getPropertyValue(p)) || 0; };
  var sliders = [
    ['seaml','--seaml','%',0,40,0.5],
    ['seamr','--seamr','%',0,40,0.5],
    ['blend','--blend','px',0,90,1]
  ];
  // per-feed position / roll — set by dragging, not sliders
  var poseKeys = ['dxl','dyl','rotl','dxr','dyr','rotr'];
  var isDeg = function(k){ return /^rot/.test(k); };
  var st = {};
  poseKeys.forEach(function(k){
    var q = qp.get(k);
    st[k] = parseFloat(q !== null ? q : num('--'+k)) || 0;
  });

  var panel = document.createElement('div');
  panel.id = 'cal';
  panel.innerHTML = sliders.map(function(k){
    var q = qp.get(k[0]);
    var cur = parseFloat(q !== null ? q : num(k[1])) || 0;
    return '<label>'+k[0]+' <input type="range" name="'+k[0]+'" min="'+k[3]+
      '" max="'+k[4]+'" step="'+k[5]+'" value="'+cur+'"><output></output></label>';
  }).join('') +
    '<div class="cal-hint">arraste = mover · shift+arrasta = girar</div>' +
    '<div id="cal-xy"></div>' +
    '<label><input type="checkbox" id="cal-raw"> ver cru (zera tudo)</label>' +
    '<label><input type="checkbox" id="cal-guide" checked> guia no centro</label>' +
    '<button type="button" id="cal-reset">zerar</button>' +
    '<div id="cal-url"></div>';
  document.body.appendChild(panel);

  var guide = document.createElement('div');
  guide.id = 'cal-guideline';
  (document.querySelector('.strip') || document.body).appendChild(guide);

  function apply(){
    var raw = document.getElementById('cal-raw').checked;
    var parts = [];
    sliders.forEach(function(k){
      var inp = panel.querySelector('input[name="'+k[0]+'"]');
      var v = raw ? 0 : parseFloat(inp.value);
      document.documentElement.style.setProperty(k[1], v + k[2]);
      inp.nextElementSibling.textContent = v;
      parts.push(k[0]+'='+(raw ? 0 : inp.value));
    });
    poseKeys.forEach(function(k){
      var v = raw ? 0 : st[k];
      document.documentElement.style.setProperty('--'+k, v + (isDeg(k)?'deg':'px'));
      parts.push(k+'='+(+v.toFixed(isDeg(k)?2:0)));
    });
    guide.hidden = !document.getElementById('cal-guide').checked;
    document.getElementById('cal-xy').textContent =
      'CAM1 '+st.dxl.toFixed(0)+','+st.dyl.toFixed(0)+' ∠'+st.rotl.toFixed(2)+
      '   CAM2 '+st.dxr.toFixed(0)+','+st.dyr.toFixed(0)+' ∠'+st.rotr.toFixed(2);
    document.getElementById('cal-url').textContent = '?' + parts.join('&');
  }
  panel.addEventListener('input', apply);
  document.getElementById('cal-reset').addEventListener('click', function(){
    poseKeys.forEach(function(k){ st[k] = 0; });
    sliders.forEach(function(k){ panel.querySelector('input[name="'+k[0]+'"]').value = 0; });
    apply();
  });

  // drag a feed to move it; shift-drag rolls it
  var strip = document.querySelector('.strip');
  var drag = null;
  if (strip) {
  strip.addEventListener('pointerdown', function(e){
    var fig = e.target.closest('figure'); if (!fig) return;
    var sfx = fig.dataset.side === 'left' ? 'l' : 'r';
    drag = {sfx:sfx, x:e.clientX, y:e.clientY, rot:e.shiftKey,
            dx0:st['dx'+sfx], dy0:st['dy'+sfx], rot0:st['rot'+sfx]};
    strip.setPointerCapture(e.pointerId);
    e.preventDefault();
  });
  strip.addEventListener('pointermove', function(e){
    if (!drag) return;
    var d = e.clientX - drag.x;
    if (drag.rot) st['rot'+drag.sfx] = drag.rot0 + d * 0.05;
    else {
      st['dx'+drag.sfx] = drag.dx0 + d;
      st['dy'+drag.sfx] = drag.dy0 + (e.clientY - drag.y);
    }
    apply();
  });
  strip.addEventListener('pointerup', function(){ drag = null; });
  strip.addEventListener('pointercancel', function(){ drag = null; });
  }

  apply();
}

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

// ── shared play gate: one control drives both feeds ──
// applyPlayState only touches videos not already in the target state, so the
// resulting play/pause events re-enter as no-ops — no feedback loop.
var gate = document.querySelector('.playgate');
if (gate && players.length === 2) {
  var wantPlaying = false;
  function applyPlayState(){
    players.forEach(function(p){
      if (wantPlaying && p.v.paused) { var r = p.v.play(); if (r && r.catch) r.catch(function(){}); }
      else if (!wantPlaying && !p.v.paused) { p.v.pause(); }
    });
    gate.hidden = wantPlaying;
  }
  players.forEach(function(p){
    p.v.addEventListener('play',  function(){ if (!wantPlaying) { wantPlaying = true;  applyPlayState(); } });
    p.v.addEventListener('pause', function(){ if (wantPlaying)  { wantPlaying = false; applyPlayState(); } });
  });
  gate.addEventListener('click', function(){ wantPlaying = true; applyPlayState(); });
}

// ── fullscreen: CSS immersive overlay everywhere, native FS + landscape
// lock where the browser allows it (iOS Safari gets the overlay only) ──
var fsBtn = document.querySelector('.fs-btn');
if (fsBtn) {
  var fsEl = function(){ return document.fullscreenElement || document.webkitFullscreenElement; };
  function enterFs(){
    document.body.classList.add('immersive');
    fsBtn.setAttribute('aria-pressed', 'true');
    var el = document.documentElement, req = el.requestFullscreen || el.webkitRequestFullscreen;
    if (req) { try { var p = req.call(el); if (p && p.catch) p.catch(function(){}); } catch(e){} }
    try { if (screen.orientation && screen.orientation.lock) screen.orientation.lock('landscape').catch(function(){}); } catch(e){}
  }
  function exitFs(){
    document.body.classList.remove('immersive');
    fsBtn.setAttribute('aria-pressed', 'false');
    var ex = document.exitFullscreen || document.webkitExitFullscreen;
    if (ex && fsEl()) { try { ex.call(document); } catch(e){} }
    try { if (screen.orientation && screen.orientation.unlock) screen.orientation.unlock(); } catch(e){}
  }
  fsBtn.addEventListener('click', function(){
    document.body.classList.contains('immersive') ? exitFs() : enterFs();
  });
  // user left native fullscreen via Esc / system back -> drop the overlay too
  var onFsChange = function(){ if (!fsEl() && document.body.classList.contains('immersive')) exitFs(); };
  document.addEventListener('fullscreenchange', onFsChange);
  document.addEventListener('webkitfullscreenchange', onFsChange);
}

if (qp.get('selftest') === '1') {
  console.assert(correction(5).seek === -5 && correction(5).rate === 1, 'big drift -> seek back');
  console.assert(correction(1).seek === 0 && correction(1).rate === 0.97, 'small drift -> ease');
  console.assert(correction(0.05).seek === 0 && correction(0.05).rate === 1, 'aligned -> rate 1');
  setLen('--selftest', '12', 'px', 0);
  console.assert(getComputedStyle(document.documentElement).getPropertyValue('--selftest').trim() === '12px', 'setLen number -> unit');
  setLen('--selftest', '-4', 'px', 0);
  console.assert(getComputedStyle(document.documentElement).getPropertyValue('--selftest').trim() === '0px', 'setLen respects min');
  console.log('selftest ok');
}
</script>
{{else}}
<p class="empty">Nenhuma câmera ao vivo no momento.</p>
{{end}}
<footer class="ftr">
 <div>Campo Society Viana <span class="gold">·</span> CHAPECÓ <span class="gold">/</span> SC</div>
 <div class="ftr-sub">camposocietyviana <span class="gold">·</span> <span id="yr"></span></div>
</footer>
<script>var y=document.getElementById('yr');if(y)y.textContent=new Date().getFullYear();</script>
</body></html>`))
