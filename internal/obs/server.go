package obs

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ServeHTTP starts the status server on cfg.HTTPAddr and blocks until ctx is
// cancelled, then shuts it down. A blank HTTPAddr disables the server.
func (r *Recorder) ServeHTTP(ctx context.Context) {
	if r.cfg.HTTPAddr == "" {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/status", r.handleStatus)
	mux.HandleFunc("/events", r.handleEvents)
	mux.HandleFunc("/trigger", r.handleTrigger)
	mux.HandleFunc("/botao", r.handleBotao)
	mux.HandleFunc("/test-alert", r.handleTestAlert)
	if r.cfg.LiveDir != "" {
		mux.Handle("/live/", r.liveHandler())
		mux.Handle("/sponsors/", r.sponsorsHandler())
	}
	mux.HandleFunc("/", r.handleIndex)

	srv := &http.Server{Addr: r.cfg.HTTPAddr, Handler: mux}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.Info("obs: status server listening", slog.String("addr", r.cfg.HTTPAddr))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("obs: status server failed", slog.Any("error", err))
	}
}

func (r *Recorder) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, r.snapshot())
}

// handleTrigger fires a replay, exactly as if the physical button was pressed.
// POST only, so a page refresh or link prefetch can't trigger it. The replay's
// own re-press / queue logic (in onPress) handles rapid repeat clicks.
func (r *Recorder) handleTrigger(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if tok := r.cfg.TriggerToken; tok != "" && !triggerTokenOK(req, tok) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"error":"PIN inválido"}`))
		return
	}
	r.triggerMu.RLock()
	fn := r.trigger
	r.triggerMu.RUnlock()
	if fn == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "trigger indisponível (agente ainda subindo)"})
		return
	}
	t := time.Now().UTC()
	fn(t) // onPress records the button.press / repress_dropped / queued event itself
	writeJSON(w, map[string]any{"ok": true, "trigger": t.Format(time.RFC3339)})
}

// triggerTokenOK reports whether the request carries the shared PIN, via the
// X-Trigger-Token header (used by /botao) or a ?token= query param (handy for curl).
func triggerTokenOK(req *http.Request, want string) bool {
	got := req.Header.Get("X-Trigger-Token")
	if got == "" {
		got = req.URL.Query().Get("token")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// handleBotao serves the stripped-down phone page — one big button that POSTs to
// /trigger — for when the physical arcade button dies mid-game. It's static and
// carries no secret; the PIN (if any) is entered on the page and kept in the
// phone's localStorage.
func (r *Recorder) handleBotao(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/botao" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(botaoHTML))
}

const botaoHTML = `<!doctype html>
<html lang="pt-br"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="dark">
<title>Replay — Viana Society</title>
<style>
 *{box-sizing:border-box}html,body{height:100%}
 body{margin:0;background:#0d1117;color:#e6edf3;
  font:16px/1.5 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;
  display:flex;flex-direction:column;align-items:center;justify-content:center;
  gap:1.6rem;padding:1.5rem;text-align:center}
 h1{font-size:.9rem;font-weight:600;color:#8b949e;letter-spacing:.06em;margin:0}
 button{width:min(78vw,300px);height:min(78vw,300px);border-radius:50%;border:0;
  background:#3fb950;color:#08260f;font:800 1.7rem/1.1 system-ui,sans-serif;
  cursor:pointer;box-shadow:0 8px 28px rgba(63,185,80,.38)}
 button:active{transform:scale(.97)}button:disabled{opacity:.45}
 #msg{min-height:1.4rem;font-size:1rem;color:#8b949e;max-width:20rem}
 #msg.ok{color:#3fb950}#msg.bad{color:#f85149}
</style></head><body>
<h1>REPLAY · VIANA SOCIETY</h1>
<button id="b" onclick="go()">GERAR<br>REPLAY</button>
<div id="msg">Aperte para gerar o replay dos últimos segundos.</div>
<script>
var b=document.getElementById('b'),m=document.getElementById('msg');
function set(t,c){m.textContent=t;m.className=c||''}
function go(){
  b.disabled=true;set('enviando…');
  fetch('trigger',{method:'POST',headers:{'X-Trigger-Token':localStorage.pin||''}})
   .then(function(r){
     if(r.status===401){
       var p=prompt('PIN do botão:');
       if(p){localStorage.pin=p;return go()}
       throw new Error('PIN necessário');
     }
     return r.json();
   })
   .then(function(d){
     if(!d)return;
     if(d.ok){set('✅ replay disparado — clipe em ~30s','ok');
       setTimeout(function(){b.disabled=false;set('Pronto pra gerar outro.')},15000)}
     else{set('⚠️ '+(d.error||'falhou'),'bad');b.disabled=false}
   })
   .catch(function(e){set('⚠️ '+e.message,'bad');b.disabled=false});
}
</script>
</body></html>`

// handleTestAlert sends a one-off Telegram message so an operator can confirm
// the bot token / chat / alert thread are configured right.
func (r *Recorder) handleTestAlert(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	if r.cfg.Bot == nil {
		writeJSON(w, map[string]any{"ok": false, "error": "Telegram não configurado (REPLAY_BOT_TOKEN/REPLAY_CHAT_ID)"})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 20*time.Second)
	defer cancel()
	err := r.cfg.Bot.SendText(ctx, "🔔 teste de alerta do replay-agent — se você recebeu isto, o Telegram está ok.", r.cfg.AlertThread)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// eventRow is one row of the events table as returned by /events.
type eventRow struct {
	TS     string          `json:"ts"`
	Level  string          `json:"level"`
	Kind   string          `json:"kind"`
	Camera string          `json:"camera,omitempty"`
	Msg    string          `json:"msg,omitempty"`
	Fields json.RawMessage `json:"fields,omitempty"`
}

func (r *Recorder) handleEvents(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()

	limit := 100
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 1000 {
		limit = n
	}

	where := "1=1"
	var args []any
	if since := q.Get("since"); since != "" {
		where += " AND ts >= ?"
		args = append(args, since)
	}
	if level := q.Get("level"); level != "" {
		where += " AND level = ?"
		args = append(args, level)
	}
	if kind := q.Get("kind"); kind != "" {
		where += " AND kind = ?"
		args = append(args, kind)
	}
	args = append(args, limit)

	r.mu.Lock()
	rows, err := r.db.Query(`SELECT ts, level, kind, camera, msg, fields FROM events WHERE `+where+` ORDER BY id DESC LIMIT ?`, args...)
	r.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []eventRow{}
	for rows.Next() {
		var e eventRow
		var camera, msg, fields string
		if err := rows.Scan(&e.TS, &e.Level, &e.Kind, &camera, &msg, &fields); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		e.Camera, e.Msg = camera, msg
		if fields != "" {
			e.Fields = json.RawMessage(fields)
		}
		out = append(out, e)
	}
	writeJSON(w, out)
}

func (r *Recorder) handleIndex(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != "/" {
		http.NotFound(w, req)
		return
	}
	// ponytail: the botao.* hostname is dedicated to the phone button — serve
	// just the button there, not the whole dashboard. Any other host (LAN IP,
	// replay.*) gets the full status page.
	if strings.HasPrefix(req.Host, "botao.") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(botaoHTML))
		return
	}
	if r.cfg.LiveDir != "" && strings.HasPrefix(req.Host, "aovivo.") {
		r.handleLivePage(w, req)
		return
	}
	s := r.snapshot()

	r.mu.Lock()
	rows, err := r.db.Query(`SELECT ts, level, kind, camera, msg FROM events ORDER BY id DESC LIMIT 40`)
	r.mu.Unlock()

	var events []recentEvent
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var e recentEvent
			if rows.Scan(&e.TS, &e.Level, &e.Kind, &e.Camera, &e.Msg) == nil {
				events = append(events, e)
			}
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTmpl.Execute(w, indexData{Snapshot: s, Events: events, Problems: problems(s), Now: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		slog.Warn("obs: render index failed", slog.Any("error", err))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
