# Design — agendamento e controle manual da transmissão ao vivo

Status: aceito 2026-09-07 (brainstorming); implementado 2026-09-07 nas branches
`feature/live-schedule` (replay-site + replay-saas), sem commit.
Relacionado: `docs/design-live-stream.md` (a live em si),
`replay-site/DESIGN_ADMIN_SLOTS.md` (grade de slots).

## Understanding summary

- **O quê:** a transmissão ao vivo (FFmpeg HLS no `replay-agent`) passa a ligar
  e desligar sozinha conforme os slots cadastrados no `/admin/slots`, com um
  controle manual no painel admin (nova página "Transmissão") que sobrepõe o
  automático.
- **Por quê:** segurança — ninguém deve conseguir monitorar o campo pela live
  fora dos horários de jogo; e comodidade — não depender de editar env var no
  notebook do campo.
- **Para quem:** o admin do campo (um operador). Viewers de
  `aovivo.vianasociety.com.br` ganham uma tela informativa quando está fora do
  ar.
- **Fonte da verdade:** o backend PHP calcula o estado; o `replay-agent` faz
  poll e obedece (liga / mata o FFmpeg de verdade — libera sessão RTSP, CPU e
  uplink).
- **Janela automática:** união dos slots recorrentes do dia (`slots.weekday`) +
  jogos avulsos do dia (`games.slot_date = hoje`). Liga **15 min antes** do
  primeiro início, desliga **1 h depois** do fim do último
  (`start + duration_m`). Contínua entre o primeiro e o último — sem desligar
  nos buracos.
- **Override manual:** 3 estados no BD — `auto` (segue slots) / `force_on`
  (liga fora de hora) / `force_off` (mantém desligada mesmo em horário de
  slot). Sem expiração automática; o admin volta pra `auto` quando quiser.
- **Falha de rede:** o agent mantém o último estado conhecido até reconectar.

## Assumptions

1. Gating é **global** — todas as câmeras ligam/desligam juntas (condiz com a
   panorâmica de 2 câmeras).
2. Timezone do cálculo no backend: `America/Sao_Paulo` (env `LIVE_TZ`).
3. Poll do agent: 30 s (`REPLAY_LIVE_POLL_INTERVAL`). Precisão de liga/desliga
   ~30 s — aceitável com as margens de 15 min / 1 h.
4. `REPLAY_LIVE_ENABLED` é **retirado**. O `live_control.mode` no BD é o mestre
   único. A live "existe" no agent quando há `LiveRTSPUrl` derivável **e**
   `REPLAY_BACKEND_URL` + `REPLAY_BACKEND_API_KEY` setados.
5. A página `aovivo` pega o estado do **próprio agent** (`/live/state.json`),
   que reflete o último poll — sem endpoint público novo no site.
6. Nova tabela mínima `live_control` (1 linha). Sem tabela de settings genérica
   (não existe hoje).
7. Sem PHPUnit no backend → o check da janela é um script standalone com
   `assert()`.
8. Escala: 1 operador, ~10 slots/dia, 1 request/30 s do agent — carga
   desprezível pro PHP/MySQL.
9. Sem histórico/auditoria de mudanças além de `updated_by` / `updated_at` na
   linha única.
10. Dia sem nenhum slot nem game → live 100 % off no modo `auto` (confirmado).

## Non-functional

- **Performance / escala:** 1 poll a cada 30 s; endpoint faz 2 SELECTs
  (`slots`, `games` do dia) + 1 na `live_control`. Desprezível.
- **Segurança:** `/api/live/state` atrás do `X-Api-Key` (o mesmo do
  `POST /api/videos`); `/api/admin/live` atrás do admin token. O objetivo do
  recurso *é* segurança física (câmera desligada fora de hora).
- **Confiabilidade:** falha de rede → último estado conhecido (não derruba a
  live no meio do jogo por um blip). Agent fora do ar → sem live, mas já
  dispara alerta hoje (sem live = sem clipe).
- **Manutenção:** toda a lógica de horário num lugar só (PHP). O agent fica
  burro (poll + gate). Runbook atualizado.

## Arquitetura

```
replay-agent (systemd, sempre rodando no notebook do campo)
  ├─ goroutine liveController: a cada 30s → GET {BACKEND}/api/live/state  (X-Api-Key)
  │        └─ resposta → live.Gate.Set(on, state)
  │
  ├─ video.Engine (por câmera): runLiveHLS
  │        topo do loop → gate.WaitOn(ctx)  (bloqueia enquanto off)
  │        durante o FFmpeg → gate vira off ⇒ cancela procCtx ⇒ SIGTERM no FFmpeg
  │
  └─ internal/obs: serve aovivo.*  +  GET /live/state.json  (de gate.Snapshot())

site PHP (stateless — só responde quando perguntado)
  GET  /api/live/state    (X-Api-Key)  → {mode,on,reason,window_start,window_end,next_window_start}
  GET  /api/admin/live    (admin)      → idem + updated_at/updated_by
  PUT  /api/admin/live    (admin)      → grava live_control.mode, retorna estado recalculado
  MySQL: live_control (1 linha), slots, games
```

Não há job/cron novo. O "quem liga automaticamente" é o goroutine de poll
dentro do `replay-agent` que já roda o tempo todo pros clipes.

## Design

### 1. Backend — schema e controle manual

Migration `replay-site/backend/migrations/015_live_control.sql`:

```sql
CREATE TABLE live_control (
    id         TINYINT UNSIGNED PRIMARY KEY DEFAULT 1,
    mode       ENUM('auto','force_on','force_off') NOT NULL DEFAULT 'auto',
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    updated_by VARCHAR(64) NULL,
    CONSTRAINT chk_single_row CHECK (id = 1)
);
INSERT INTO live_control (id, mode) VALUES (1, 'auto');
```

Rotas novas em `backend/public/index.php`:

| Método | URI | Auth | Handler |
|---|---|---|---|
| `GET` | `/api/live/state` | `X-Api-Key` | `getLiveState()` |
| `GET` | `/api/admin/live` | admin token | `getAdminLiveState()` |
| `PUT` | `/api/admin/live` | admin token | `setAdminLiveMode()` |

Handlers em `backend/src/handlers/` (arquivo novo `live.php` ou dentro de
`admin.php`). `setAdminLiveMode()` valida `mode ∈ {auto,force_on,force_off}`,
`UPDATE live_control SET mode=?, updated_by=?` (login via `TokenAuth`), retorna
o estado recalculado.

Payload de `/api/live/state`:

```json
{
  "mode": "auto",
  "on": true,
  "reason": "slot",
  "window_start": "2026-09-07T17:45:00-03:00",
  "window_end":   "2026-09-07T23:00:00-03:00",
  "next_window_start": "2026-09-08T18:45:00-03:00"
}
```

`window_*` = janela de hoje já com as margens (ou `null` se hoje não tem nada).
`next_window_start` = próximo início (slot recorrente **ou** `games`) nos
próximos 7 dias, já com a margem de 15 min. `reason ∈ {slot, forced_on,
forced_off, idle}`.

### 2. Backend — cálculo da janela

Função pura em `helpers.php` (ou no handler), timezone `LIVE_TZ`:

```
liveWindowFor(DateTimeImmutable $now, array $slots, array $games): array
  → ['start' => ?DateTimeImmutable, 'end' => ?DateTimeImmutable]
```

Passos:

1. Coletar starts do dia de `$now`:
   - slots onde `weekday == diaDaSemana($now)` (converter: PHP `w` é `0=Dom`,
     a tabela é `0=Seg … 6=Dom`);
   - linhas de `games` com `slot_date == $now->format('Y-m-d')`.
   - cada um vira `{start, end = start + duration_m}`.
2. Lista vazia → `{null, null}` → `on=false`, `reason=idle`.
3. Janela = menor `start` … maior `end` (contínua, ignora buracos).
4. Margens: `start -= LIVE_MARGIN_BEFORE_MIN` (15), `end += LIVE_MARGIN_AFTER_MIN`
   (60). Lidas de `$_ENV` com default.
5. No modo `auto`, `on = window.start <= now <= window.end`.

`next_window_start`: varre `now → now+7d`, monta a mesma lista por dia, retorna
o primeiro `start - margem` que seja `> now`. Nada em 7 dias → `null`.

Resolução final do `on` (no handler):

| `mode` | `on` | `reason` |
|---|---|---|
| `force_on` | `true` | `forced_on` |
| `force_off` | `false` | `forced_off` |
| `auto` | cálculo da janela | `slot` se on, `idle` se off |

Meia-noite: um slot `23:00` dur 60 termina `00:00` do dia seguinte, +60 min de
margem = `01:00` — coberto pela margem, não por lógica cross-day. Slot que
**começa** de madrugada (`00:00`) conta no dia dele. `ponytail:` sem tratamento
de jogo que cruza a virada além da margem.

Check: `backend/tests/live_window_check.php` (assert, standalone, roda com
`php`): dia vazio; um slot; dois slots com buraco; game avulso sem slot; `now`
dentro/fora; virada de meia-noite; `next` no mesmo dia vs. próximos dias.

### 3. Agent — poller + gate

Pacote novo `internal/live` (sem deps de `obs`/`video` — evita ciclo):

```go
type State struct {
    Mode, Reason    string
    On              bool
    NextWindowStart *time.Time
    WindowEnd       *time.Time
}

type Gate struct {
    mu    sync.Mutex
    cond  *sync.Cond
    on    bool
    state State
}
func (g *Gate) Set(on bool, s State)   // poller
func (g *Gate) WaitOn(ctx context.Context) bool  // bloqueia enquanto off; false se ctx morre
func (g *Gate) IsOn() bool
func (g *Gate) Snapshot() State
```

Poller `liveController` (goroutine em `main.go`), roda quando
`cfg.LiveDir != "" && cfg.LiveRTSPUrl != "" && backendURL != ""`:

- `GET {REPLAY_BACKEND_URL}/api/live/state` com `X-Api-Key`, a cada
  `REPLAY_LIVE_POLL_INTERVAL` (default 30 s).
- sucesso → `gate.Set(resp.On, resp)`.
- erro (rede/5xx/timeout) → **não mexe no gate**; `obs.Event(Warn,
  "live.poll_failed", …)` com rate-limit.
- primeiro poll nunca respondido → gate começa `on=false`.

`video/live.go` `runLiveHLS` muda pouco: no topo de cada volta do `for`,
`if !e.liveGate.WaitOn(ctx) { return }`. Enquanto o FFmpeg roda, um watcher:
gate vira off ⇒ cancela `procCtx` (mesmo mecanismo do stall watchdog →
`SIGTERM`). Volta pro topo, `WaitOn` bloqueia. Backoff reseta ao entrar via
gate (não é falha).

`video/engine.go`: `Engine.cfg` ganha `LiveGate *live.Gate` (compartilhado
entre todas as câmeras). `Start` sobe `runLiveHLS` sempre que `LiveRTSPUrl !=
""` — o gate decide rodar.

`main.go`: remove `cfg.LiveEnabled` e o `if cfg.LiveEnabled`. `obsCfg.LiveDir`
passa a ser sempre setado. Cria um `*live.Gate`, passa pro poller, pra cada
`video.Config` e pra `obs.Config`.

Check: `internal/live/gate_test.go` — `WaitOn` bloqueia e libera no
`Set(true)`; `Set(false)` durante execução sinaliza; `ctx` cancelado solta o
`WaitOn`.

### 4. Agent — página `aovivo` fora do ar + `/live/state.json`

`obs.Config` ganha `LiveGate *live.Gate`.

Endpoint novo `GET /live/state.json` (no `mux` junto do `/live/`):

```json
{ "on": false, "reason": "idle", "next_window_start": "2026-09-08T18:45:00-03:00" }
```

De `gate.Snapshot()`. `Access-Control-Allow-Origin: *`, `Cache-Control:
no-cache`. A página consome isso; **não** fala com o site.

`handleLivePage` / `liveTmpl`:

- mostrar player só quando `gate.IsOn() && len(liveCameras()) > 0`.
- `!on` → bloco central "Transmissão fora do ar" + (se `next_window_start`)
  "Volta {dia} às {HH:MM}" em pt-BR, sistema navy/gold. Sem player, sem hls.js.
- `on` mas 0 câmeras (FFmpeg subindo, 0–15 s) → "Conectando às câmeras…".
- JS: `fetch('/live/state.json')` a cada 20 s. `off→on` → `location.reload()`;
  `on→off` → troca pro bloco fora do ar sem reload.

Cloudflare: a Cache Rule atual pega `/live/*`. `state.json` casa com o prefixo
→ precisa de exceção (Edge TTL 1 s ou bypass). Nota no runbook, sem código.

`ponytail:` `index.m3u8` órfão no tmpfs após o desligamento não é limpo — a
página não toca player com `!on`, e o próximo start do FFmpeg sobrescreve.

### 5. Frontend admin — página "Transmissão"

`src/api.js`:
```js
export const fetchAdminLive   = ()     => adminFetch('/api/admin/live')
export const setAdminLiveMode = (mode) => adminFetch('/api/admin/live', { method: 'PUT', body: JSON.stringify({ mode }) })
```

`src/router.js`: `{ path: 'live', component: AdminLiveView }` em `/admin`.
`AdminLayout.vue`: `<RouterLink to="/admin/live" title="Transmissão">` com
ícone (padrão icon-only mobile já existe).

`AdminLiveView.vue` (uma view, sem componente novo):

- estado grande no topo: pílula `AO VIVO` (gold) / `FORA DO AR` (navy) + motivo
  em texto:
  - `slot` → "No ar — horário de jogo (até 23:00)"
  - `forced_on` → "No ar — ligada manualmente"
  - `forced_off` → "Desligada manualmente"
  - `idle` → "Fora do ar — sem jogo agora. Próximo: sábado 14:00"
- 3 botões segmentados: `Automático` / `Forçar ligado` / `Forçar desligado`.
  Ativo = `mode`. Clicar → `setAdminLiveMode()` → re-render com a resposta.
  `confirm()` só em `Forçar desligado` durante janela ativa.
- rodapé: "Alterado por {updated_by} em {updated_at}".
- polling: `fetchAdminLive()` a cada 15 s enquanto a aba está visível
  (`visibilitychange` pausa).
- loading: skeleton da pílula; erro: banner `.adm-error`. Sem tabela.

`ponytail:` sem gráfico, sem histórico, sem preview de câmera embutido (link
"abrir aovivo.*" em nova aba basta).

### 6. Config e migração

| Onde | Var | Default |
|---|---|---|
| backend PHP | `LIVE_MARGIN_BEFORE_MIN` | `15` |
| backend PHP | `LIVE_MARGIN_AFTER_MIN` | `60` |
| agent | `REPLAY_LIVE_POLL_INTERVAL_S` | `30` |

> Implementação: o backend usa o `TZ` que ele já configura globalmente
> (`America/Sao_Paulo`) em vez de um `LIVE_TZ` dedicado. O env do poll ficou
> `REPLAY_LIVE_POLL_INTERVAL_S` (segundos), pra casar com `REPLAY_SEGMENT_TIME_S`
> etc.

Retirada do `REPLAY_LIVE_ENABLED`:

- `main.go` remove `cfg.LiveEnabled` e o `if`. A live sobe quando
  `LiveRTSPUrl` sai não-vazio da derivação `subtype=0→1` (ou
  `REPLAY_CAM_<n>_LIVE_RTSP_URL`) **e** `REPLAY_BACKEND_URL` +
  `REPLAY_BACKEND_API_KEY` setados. Faltando backend → live não sobe, warn.
- migração de deploy: tirar `REPLAY_LIVE_ENABLED=true` do `.env` do notebook é
  opcional (var ignorada). `live_control` nasce `auto` → a live passa a seguir
  os slots no primeiro deploy.
- `RUNBOOK_JOGO.md`: seção "Transmissão ao vivo" perde o passo `REPLAY_LIVE_ENABLED`,
  ganha "controle em `/admin` → Transmissão"; nota da exceção de cache pro
  `state.json`.

### Edge cases consolidados

| Caso | Comportamento |
|---|---|
| Primeiro poll nunca respondeu | live off (fail-safe), warn |
| Poll falha no meio do jogo | mantém último estado (segue no ar) |
| `force_off` durante janela | desliga; `confirm()` na UI |
| Slot criado/editado enquanto no ar | próximo poll (≤30 s) recalcula |
| Slot 23:00 dur 60 | termina 00:00 +1h margem = 01:00; coberto pela margem |
| Dia sem slot nem game | off o dia todo no `auto` |
| FFmpeg cai dentro da janela | retry/backoff atual, gate segue `on` |
| tmpfs cheio / sem `LiveDir` | igual hoje — `runLiveHLS` loga e sai |
| Cloudflare cacheia `state.json` | Cache Rule ganha exceção (Edge TTL 1 s) — doc |
| `mode` inválido no PUT | 400, não grava |

### Testes

- PHP: `backend/tests/live_window_check.php` (assert, standalone).
- Go: `internal/live/gate_test.go`; opcional teste do parser da resposta do
  poller.
- Manual: `force_on` fora de hora liga em ≤30 s; criar slot pra "daqui a
  20 min" e ver ligar 15 min depois (5 min antes do slot); `force_off` no meio
  de uma janela corta a live; derrubar a rede do notebook e ver a live seguir.

## Decision log

| # | Decisão | Alternativas | Por quê |
|---|---|---|---|
| 1 | Site calcula o estado, agent obedece via poll | agent calcula (busca slots); agent usa só o `agenda.json` local | lógica de horário e botão admin num lugar só; sem duplicar janela em Go e PHP; `agenda.json` não tem duração nem override |
| 2 | 3 estados no BD: `auto` / `force_on` / `force_off` | override com expiração; toggle simples sem auto | cobre todos os casos sem timer; `auto` é o objetivo (não monitorar) |
| 3 | `REPLAY_LIVE_ENABLED` retirado; `live_control.mode` é o mestre | manter a env como kill-switch local | pedido explícito — "passamos a usar os estados controláveis via BD"; um mestre só |
| 4 | Janela = slots recorrentes ∪ games do dia | só recorrentes; só games | jogo extra fora da grade fixa também liga a câmera |
| 5 | Contínua entre 1º e último slot (ignora buracos) | desligar nos buracos; desligar só em buraco grande | "roda até o último jogo"; menos liga-desliga do FFmpeg; menos borda |
| 6 | Falha de poll → mantém último estado | fail-safe off; fallback pro `agenda.json` | blip de rede não derruba a live no meio do jogo; sem lógica duplicada |
| 7 | Margens: 15 min antes, 1 h depois (env) | 5/0; 10/15 | escolha do dono — pega setup antes e prorrogação/saída depois |
| 8 | Gate com `sync.Cond` + `procCtx` cancelado (supervisor) | coordinator externo cria/destrói goroutines; bool atômico no loop | diff mínimo no `runLiveHLS`; mata FFmpeg pelo `CommandContext` que já existe; sem leak em transição rápida |
| 9 | Página `aovivo` lê `/live/state.json` do agent | endpoint público no site; page fala direto com o site | o agent já tem o estado; zero auth nova; funciona offline do site |
| 10 | Tabela `live_control` de 1 linha | tabela de settings genérica | YAGNI — só um valor |
| 11 | Gating global (todas as câmeras juntas) | por câmera | panorâmica de 2 câmeras é uma coisa só |
| 12 | Página admin própria "Transmissão" | card no dashboard; botão no `/admin/slots` | separa config recorrente de ação operacional; dashboard já cheio |
| 13 | Sem histórico de mudanças além de `updated_by`/`updated_at` | tabela de auditoria | 1 operador; YAGNI |

## Não-objetivos

- Agendar por câmera individualmente.
- Timer/expiração no override manual.
- Histórico/auditoria de quem ligou/desligou.
- Preview de câmera dentro do painel admin.
- Placeholder em vídeo/loop na tela fora do ar (só texto).
- Sincronizar `agenda.json` do agent com a tabela `slots` do site.
- Tratamento de jogo que cruza a meia-noite além da margem.
- Mudança no pipeline de clipes ou no `agenda.json`.
