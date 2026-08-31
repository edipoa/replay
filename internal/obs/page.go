package obs

import (
	"html/template"
	"strconv"
)

type recentEvent struct{ TS, Level, Kind, Camera, Msg string }

type indexData struct {
	Snapshot Snapshot
	Events   []recentEvent
	Problems []string // empty = agent fully healthy; drives the status banner
	Now      string
}

// problems lists everything currently wrong, roughly worst-first. It reads only
// the snapshot the page already has, so the banner never disagrees with the
// cards below it.
func problems(s Snapshot) []string {
	var p []string
	if !s.USBOK {
		p = append(p, "botão USB desconectado")
	}
	for _, c := range s.Cameras {
		if !c.Up {
			p = append(p, c.ID+" sem sinal")
		}
	}
	if s.DiskFreePct >= 0 && s.DiskFreePct < 10 {
		p = append(p, "disco quase cheio ("+strconv.Itoa(s.DiskFreePct)+"%)")
	}
	if s.ClipsFailedToday > 0 {
		p = append(p, strconv.Itoa(s.ClipsFailedToday)+" falha(s) de clipe hoje")
	}
	for _, g := range s.AlertsFiring {
		// camera/usb groups are already covered above; skip the duplicate.
		if g == "usb" || len(g) > 7 && g[:7] == "camera/" {
			continue
		}
		p = append(p, "alerta ativo: "+g)
	}
	return p
}

// indexTmpl is the whole "/" page — one self-contained HTML doc, no external
// assets, so it works on the venue's flaky connection and while the agent is
// restarting. The page reloads itself every 10s (paused for 30s after any
// button press so the result stays readable).
var indexTmpl = template.Must(template.New("index").Parse(`<!doctype html>
<html lang="pt-br"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark">
<title>replay-agent status</title>
<style>
 :root{
  --bg:#0d1117;--surface:#161b22;--raised:#1c2128;--border:#30363d;
  --text:#e6edf3;--muted:#8b949e;
  --ok:#3fb950;--bad:#f85149;--warn:#d29922;--accent:#58a6ff;
 }
 *{box-sizing:border-box}
 body{font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
  margin:0;padding:1rem;max-width:720px;margin-inline:auto;
  background:var(--bg);color:var(--text)}
 header{display:flex;align-items:baseline;gap:.5rem;flex-wrap:wrap;margin-bottom:.9rem}
 h1{font-size:1.05rem;margin:0;letter-spacing:.01em}
 h2{font-size:.8rem;text-transform:uppercase;letter-spacing:.08em;
  color:var(--muted);margin:1.4rem 0 .6rem}
 .sub{color:var(--muted);font-size:.8rem}
 a.refresh{margin-left:auto;color:var(--accent);text-decoration:none;font-size:.85rem}

 /* status banner */
 .banner{border-radius:10px;padding:.8rem 1rem;margin-bottom:1rem;
  border:1px solid var(--border);border-left-width:4px;background:var(--surface)}
 .banner.good{border-left-color:var(--ok)}
 .banner.bad{border-left-color:var(--bad)}
 .banner .headline{font-weight:600;font-size:1.05rem;display:flex;align-items:center;gap:.5rem}
 .banner.good .headline{color:var(--ok)}
 .banner.bad .headline{color:var(--bad)}
 .banner ul{margin:.5rem 0 0;padding-left:1.2rem}
 .banner li{margin:.15rem 0}

 /* actions */
 .actions{display:flex;flex-wrap:wrap;gap:.5rem;align-items:center;margin-bottom:.4rem}
 button.act{font:600 1rem system-ui,sans-serif;color:#fff;border:0;border-radius:8px;
  min-height:48px;padding:.6rem 1.1rem;cursor:pointer;flex:1 1 auto}
 button.act:disabled{opacity:.5;cursor:default}
 #trig{background:var(--ok);color:#08260f}
 #talert{background:transparent;border:1px solid var(--border);color:var(--text);
  font-weight:400;font-size:.9rem;flex:0 1 auto}
 #trigmsg{display:block;min-height:1.2rem;color:var(--muted);font-size:.9rem;margin:.2rem 0 .2rem}

 /* stat + camera cards */
 .grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:.5rem}
 .card{background:var(--surface);border:1px solid var(--border);border-radius:10px;padding:.65rem .8rem}
 .card .label{font-size:.72rem;text-transform:uppercase;letter-spacing:.06em;color:var(--muted)}
 .card .val{display:block;font-size:1.5rem;font-weight:600;margin-top:.15rem;line-height:1.2}
 .card .val.sm{font-size:.95rem;font-weight:500}
 .card .note{font-size:.78rem;color:var(--muted)}
 .ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}
 .dot{display:inline-block;width:.6rem;height:.6rem;border-radius:50%;margin-right:.35rem;vertical-align:.05em}
 .dot.ok{background:var(--ok)}.dot.bad{background:var(--bad);animation:pulse 1.4s ease-in-out infinite}

 /* events */
 .events{overflow-x:auto;border:1px solid var(--border);border-radius:10px}
 table{border-collapse:collapse;width:100%;font-size:.82rem}
 th{position:sticky;top:0;background:var(--raised);text-align:left;
  font-size:.7rem;text-transform:uppercase;letter-spacing:.05em;color:var(--muted)}
 td,th{padding:.4rem .6rem;white-space:nowrap;vertical-align:top}
 tbody tr{border-top:1px solid var(--border);border-left:3px solid transparent}
 tr.lvl-warn{border-left-color:var(--warn)}
 tr.lvl-critical{border-left-color:var(--bad)}
 tr.hb td{color:var(--muted)}
 td.msg{white-space:normal;max-width:22rem}
 .badge{font-size:.68rem;text-transform:uppercase;letter-spacing:.04em;
  padding:.05rem .4rem;border-radius:4px;background:var(--raised);color:var(--muted)}
 .badge.warn{background:rgba(210,153,34,.18);color:var(--warn)}
 .badge.critical{background:rgba(248,81,73,.18);color:var(--bad)}

 @keyframes pulse{0%,100%{opacity:1}50%{opacity:.35}}
 @media (prefers-reduced-motion:reduce){*{animation:none!important}}
</style></head><body>

<header>
 <h1>replay-agent</h1>
 <span class="sub t">{{.Now}}</span>
 <a class="refresh" href="#" onclick="location.reload();return false">↻ atualizar</a>
</header>

{{if .Problems}}
<div class="banner bad">
 <div class="headline"><span class="dot bad"></span>Precisa de atenção</div>
 <ul>{{range .Problems}}<li>{{.}}</li>{{end}}</ul>
</div>
{{else}}
<div class="banner good">
 <div class="headline"><span class="dot ok"></span>Tudo funcionando</div>
</div>
{{end}}

<div class="actions">
 <button id="trig" class="act" onclick="trigger()">Gerar replay agora</button>
 <button id="talert" class="act" onclick="testAlert()">testar alerta</button>
</div>
<span id="trigmsg"></span>

<h2>agente</h2>
<div class="grid">
 <div class="card"><span class="label">uptime</span><span class="val">{{.Snapshot.UptimeS}}s</span></div>
 <div class="card"><span class="label">botão USB</span><span class="val {{if .Snapshot.USBOK}}ok{{else}}bad{{end}}">{{if .Snapshot.USBOK}}ok{{else}}caído{{end}}</span></div>
 <div class="card"><span class="label">clipes hoje</span><span class="val">{{.Snapshot.ClipsToday}}</span></div>
 <div class="card"><span class="label">falhas hoje</span><span class="val {{if .Snapshot.ClipsFailedToday}}bad{{end}}">{{.Snapshot.ClipsFailedToday}}</span></div>
 <div class="card"><span class="label">disco livre</span><span class="val {{if lt .Snapshot.DiskFreePct 10}}bad{{else if lt .Snapshot.DiskFreePct 20}}warn{{end}}">{{.Snapshot.DiskFreePct}}%</span></div>
 <div class="card"><span class="label">último clipe</span><span class="val sm t">{{if .Snapshot.LastClipAt}}{{.Snapshot.LastClipAt}}{{else}}—{{end}}</span></div>
</div>

<h2>câmeras</h2>
<div class="grid">
{{range .Snapshot.Cameras}}
 <div class="card">
  <span class="label">{{.ID}}</span>
  <span class="val {{if .Up}}ok{{else}}bad{{end}}"><span class="dot {{if .Up}}ok{{else}}bad{{end}}"></span>{{if .Up}}up{{else}}sem sinal{{end}}</span>
  <span class="note">{{if lt .BufferAgeS 0}}nenhum segmento ainda{{else}}último segmento há {{.BufferAgeS}}s{{end}}</span>
 </div>
{{end}}
</div>

<h2>últimos eventos</h2>
<div class="events">
<table>
<thead><tr><th>quando</th><th>nível</th><th>evento</th><th>cam</th><th>mensagem</th></tr></thead>
<tbody>
{{range .Events}}
<tr class="lvl-{{.Level}}{{if eq .Kind "heartbeat"}} hb{{end}}">
 <td class="t">{{.TS}}</td>
 <td><span class="badge {{.Level}}">{{.Level}}</span></td>
 <td>{{.Kind}}</td>
 <td>{{.Camera}}</td>
 <td class="msg">{{if .Msg}}{{.Msg}}{{end}}</td>
</tr>
{{end}}
</tbody>
</table>
</div>

<script>
// Timestamps are stored/served in UTC; render them in the viewer's local time.
for (var el of document.querySelectorAll('.t')) {
  var d = new Date(el.textContent.trim());
  if (!isNaN(d)) el.textContent = d.toLocaleString();
}

// Self-reload every 10s, but hold off for 30s after a button press (and while
// the tab is hidden) so the operator can read the result.
var pausedUntil = 0;
(function loop(){
  setTimeout(function(){
    if (Date.now() < pausedUntil || document.hidden) { loop(); return; }
    location.reload();
  }, 10000);
})();

function post(path, btnId, okMsg) {
  pausedUntil = Date.now() + 30000;
  var b = document.getElementById(btnId), m = document.getElementById('trigmsg');
  b.disabled = true; m.textContent = '…'; m.className = '';
  fetch(path, { method: 'POST', headers: { 'X-Trigger-Token': localStorage.pin || '' } })
    .then(function(r){
      if (r.status === 401) {
        var p = prompt('PIN do botão:');
        if (p) { localStorage.pin = p; b.disabled = false; return post(path, btnId, okMsg); }
        throw new Error('PIN necessário');
      }
      return r.json();
    })
    .then(function(d){
      if (!d) return; // 401 retry path handled its own UI
      m.textContent = d.ok ? okMsg(d) : '⚠️ ' + (d.error || 'falhou');
      m.className = d.ok ? 'ok' : 'bad';
    })
    .catch(function(e){ m.textContent = '⚠️ ' + e; m.className = 'bad'; })
    .finally(function(){ setTimeout(function(){ b.disabled = false; }, 5000); });
}
function trigger() {
  post('trigger', 'trig', function(d){
    return '✅ disparado ' + new Date(d.trigger).toLocaleTimeString() + ' — clipe em ~30s';
  });
}
function testAlert() {
  post('test-alert', 'talert', function(){
    return '✅ mensagem de teste enviada — confere o Telegram';
  });
}
</script>
</body></html>`))
