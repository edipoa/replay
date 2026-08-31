# Runbook — Validação no Jogo

Checklist completo para colocar o sistema de replay funcionando no campo.

Máquina de campo: Ubuntu Server (headless, sem GNOME) rodando o
`replay-agent` nativo como serviço systemd — sobe sozinho no boot, sem
precisar abrir terminal no dia do jogo.

---

## SETUP ÚNICO (uma vez, antes do primeiro jogo — não repete a cada partida)

### 1. Copiar o pacote pro notebook

Copiar `dist/linux/` (ou `dist/replay-agent-linux.zip` descompactado) para
a máquina, ex: `/home/replay/replay-agent`.

---

### 2. Configurar `.env` e testar as câmeras

```bash
cd /home/replay/replay-agent
./setup.sh
```

Preenche RTSP das câmeras, R2/backend (se usados) e já testa a conexão com
cada câmera.

---

### 3. Impedir suspensão (equivalente ao antigo `gsettings`, mas sem GNOME)

Ubuntu Server não tem GNOME, então em vez dos comandos `gsettings` de antes,
mexe direto no systemd. É configuração de máquina, feita uma vez só:

```bash
# Ignorar o fechamento da tampa do notebook
sudo sed -i 's/^#\?HandleLidSwitch=.*/HandleLidSwitch=ignore/' /etc/systemd/logind.conf
sudo systemctl restart systemd-logind

# Travar suspensão/hibernação do sistema
sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
```

---

### 4. Permissão para ler o joystick/botão arcade

O usuário que vai rodar o serviço precisa estar no grupo `input` para ler
`/dev/input/js{id}` sem ser root:

```bash
sudo usermod -aG input replay   # troque "replay" pelo usuário real
```

(Precisa relogar/reiniciar pra grupo entrar em vigor.)

---

### 5. Instalar e habilitar o serviço systemd

Editar `replay-agent.service` (dentro do pacote copiado) se o path ou o
usuário forem diferentes de `/home/replay/replay-agent` / `replay`, depois:

```bash
sudo cp replay-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable replay-agent
```

O serviço passa a subir sozinho em todo boot, com restart automático se
cair (`Restart=always`).

---

### 6. Testar o gatilho antes de confiar nele

> **Importante:** rodando como serviço headless, `REPLAY_SIMULATE=true`
> (gatilho por tecla Enter) **não funciona** — não existe terminal
> interativo anexado ao serviço. Em produção o gatilho é sempre o botão
> físico do joystick/arcade (`REPLAY_JOYSTICK_ID`).

Pra validar o fluxo completo sem o botão físico, rode manualmente em
foreground (fora do systemd, uma vez, antes do jogo):

```bash
sudo systemctl stop replay-agent   # evita concorrer pela câmera/porta
REPLAY_SIMULATE=true ./start.sh
# pressiona Enter no terminal, confere se o clipe chega no Telegram
sudo systemctl start replay-agent  # volta ao modo normal
```

No dia do jogo, teste o botão físico de verdade (ver passo 3 do "DIA DO
JOGO" abaixo).

---

### 7. Liberar restart sem senha (necessário para `make deploy`)

O deploy via SSH reinicia o serviço remotamente; sem isso o `sudo
systemctl restart` pede senha e trava o comando. Configuração de máquina,
uma vez só:

```bash
echo '<usuário> ALL=(ALL) NOPASSWD: /usr/bin/systemctl restart replay-agent' | sudo tee /etc/sudoers.d/replay-agent-restart
sudo chmod 440 /etc/sudoers.d/replay-agent-restart
```

Troque `<usuário>` pelo usuário que roda o serviço no notebook. Confirme
o path do `systemctl` com `which systemctl` (normalmente
`/usr/bin/systemctl` no Ubuntu Server) — se for outro, ajuste a linha
acima.

---

## ATUALIZAR O SISTEMA

Sempre que houver mudança no código, com o notebook na mesma rede local
que sua máquina de dev (antes de sair para o campo — **não** funciona via
tethering do celular em campo):

```bash
make deploy HOST=192.168.x.x DEPLOY_USER=<usuário>
```

Compila o binário, envia por SSH e reinicia o `replay-agent` sozinho.
Requer que a chave SSH já esteja autorizada no notebook (`ssh
<usuário>@HOST` sem pedir senha) e o passo 7 do setup único feito.

Não atualiza `.env`, `agenda.json`, ffmpeg ou watermark — só o binário.
Se algum desses mudar, copie manualmente e reinicie o serviço.

---

## DIA DO JOGO

### 1. Conectar o notebook no WiFi do campo

Conectar normalmente e verificar que tem acesso.

---

### 2. Conectar celular via USB Tethering

1. Plugar o cabo USB no celular e no notebook
2. No celular: **Configurações → Rede → Ponto de acesso → Tethering USB** → ativar
3. No notebook, confirmar que a interface apareceu:

```bash
ip addr show usb0
```

Deve aparecer um IP na faixa `192.168.42.x`. O **celular sempre fica em `192.168.42.1`**.

---

### 3. Abrir o IP Webcam no celular

1. Abrir o app **IP Webcam**
2. Rolar até o final → **"Iniciar servidor"**
3. Anotar a porta (padrão: **8080**)
4. O stream RTSP do celular vai estar em: `rtsp://192.168.42.1:8080/video`

Testar no notebook se o stream abre:

```bash
ffplay rtsp://192.168.42.1:8080/video
```

Se abrir a imagem da câmera, está ok. Fechar o ffplay (`q`).

Se a URL mudou desde o setup, atualizar o `.env` e reiniciar o serviço:

```bash
nano /home/replay/replay-agent/.env    # ajustar REPLAY_CAM_1_RTSP_URL
sudo systemctl restart replay-agent
```

---

### 4. Conferir que o serviço está rodando

O `replay-agent` já deve estar de pé sozinho desde o boot:

```bash
systemctl status replay-agent
```

Acompanhar logs ao vivo (equivalente a "olhar o terminal" do fluxo antigo):

```bash
journalctl -u replay-agent -f
```

**Painel de status ao vivo** (mais palpável que o journald): abra no navegador
do celular, na mesma LAN:

```
http://<ip-do-notebook>:8088/
```

Mostra câmeras up/down, idade do buffer, USB do botão, clipes hoje, falhas,
disco livre e alertas ativos — atualiza sozinho a cada 10s. Da sua máquina:

```bash
make status HOST=<ip-do-notebook>
curl -s http://<ip-do-notebook>:8088/events?limit=30 | jq   # timeline de eventos
```

Alertas críticos (câmera caída, ingestão travada, USB do botão desconectado,
clipe/entrega falhando) chegam sozinhos no Telegram, com mensagem de
"recuperado" quando volta.

**Botão digital de emergência** (`/botao`): se o botão físico falhar no meio do
jogo, qualquer celular abre uma página com um botão gigante que dispara o
replay igual ao físico.

- Na LAN: `http://<ip-do-notebook>:8088/botao`.
- Pela internet: `botao.vianasociety.com.br` (ver abaixo).

Fica aberto (sem PIN) por padrão — pra uma solução temporária de dia de jogo
tá ok: o `onPress` descarta re-press <15s e a fila tem cap 1, então o pior caso
de alguém achar a URL é um clipe extra de vez em quando. Se quiser um PIN,
defina `REPLAY_TRIGGER_TOKEN` no `.env` (digitado uma vez, fica salvo no
celular).

O notebook já tem um tunnel `cloudflared` (`/etc/cloudflared/config.yml`,
domínio `vianasociety.com.br`) servindo `replay.vianasociety.com.br` (painel,
atrás do Cloudflare Access). Pra publicar `botao.vianasociety.com.br`:

```bash
# 1. rota DNS pro mesmo tunnel
cloudflared tunnel route dns <nome-ou-id-do-tunnel> botao.vianasociety.com.br

# 2. adicione o hostname no ingress do /etc/cloudflared/config.yml,
#    ANTES da regra catch-all:
#   ingress:
#     - hostname: replay.vianasociety.com.br
#       service: http://localhost:8088
#     - hostname: botao.vianasociety.com.br
#       service: http://localhost:8088
#     - service: http_status:404

sudo systemctl restart cloudflared
```

**Importante:** `botao.vianasociety.com.br` NÃO pode ficar atrás do Cloudflare
Access — o pessoal no local precisa abrir direto no celular. O painel `/` e
`/events` também respondem nesse hostname; se algum dia quiser fechar o botão,
é o `REPLAY_TRIGGER_TOKEN` que faz isso (só o `POST /trigger` valida o PIN; a
página `/botao` só pede e repassa).

---

### 5. Testar antes do jogo começar

1. Apontar a câmera para o campo
2. Aguardar ~10 segundos (buffer precisa de alguns segmentos)
3. Apertar o **botão físico** do joystick/arcade
4. Aguardar ~20-30 segundos
5. Verificar se o vídeo chegou no grupo do Telegram

---

## DURANTE O JOGO

- Apertar o **botão físico** para gerar um replay
- Acompanhar erros via `journalctl -u replay-agent -f`, se necessário
- Não é preciso manter terminal aberto nem se preocupar com suspensão — o
  serviço roda em background e reinicia sozinho se cair

---

## APÓS O JOGO

Desativar o Tethering USB no celular.

### Puxar o log do jogo pra investigar depois

Todo evento fica gravado num SQLite local (`<REPLAY_OUTPUT_DIR>/replay.db`,
retido 30 dias). Pra reconstruir o que aconteceu:

```bash
scp <user>@<ip-do-notebook>:<REPLAY_OUTPUT_DIR>/replay.db .
sqlite3 replay.db \
  "select ts,level,kind,camera,msg from events order by id desc limit 100"
# só os problemas:
sqlite3 replay.db \
  "select ts,kind,camera,msg from events where level='critical' order by id"
```

Não é preciso reverter a configuração de suspensão (`logind.conf` / masks
de systemd) — é setup de máquina, feito uma vez só, permanece assim entre
jogos.

---

## Diagnósticos rápidos

| Problema | O que checar |
|---|---|
| `ffplay` não abre o stream | IP Webcam iniciado? `ip addr show usb0` existe? |
| `systemctl status replay-agent` mostra `failed` | `journalctl -u replay-agent -e` para ver o erro; `.env` salvo corretamente? |
| Botão do joystick não gera replay | Usuário do serviço está no grupo `input`? `groups replay` |
| Replay não chega no Telegram | `agenda.json` tem o horário de hoje? Ver `/tmp/telegram.log` |
| `usb0` não aparece | Desativar e reativar Tethering USB no celular |
| "Apertou mas não saiu clipe" | Abrir `http://<ip>:8088/` — ver se câmera está `up`, USB `ok`, e procurar o press em `/events` (pode ter sido `button.debounced` ou `button.repress_dropped`) |
| Clipe não chegou no site/R2 | Normal se a internet estiver ruim — fica na fila de retry (`<OUTPUT_DIR>/r2_queue/*.json`) e sobe sozinho quando a banda voltar. Alerta `upload.stuck` no Telegram só se passar de 30min. `ls <OUTPUT_DIR>/r2_queue/` mostra o que está pendente |
| Página `:8088` não abre | Notebook e celular na mesma rede? `REPLAY_HTTP_ADDR` no `.env`? `systemctl status replay-agent` |
