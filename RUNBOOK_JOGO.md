# Runbook — Validação no Jogo

Checklist completo para colocar o sistema de replay funcionando no campo.

---

## ANTES DE SAIR DE CASA

- [ ] Cabo USB (notebook ↔ celular) na mochila
- [ ] Notebook carregado (ou carregador junto)
- [ ] `agenda.json` com os horários de hoje configurados
- [ ] Celular com o app **IP Webcam** instalado

---

## NO CAMPO — Passo a passo

### 1. Conectar notebook no WiFi do campo
Conectar normalmente. Verificar que tem acesso.

---

### 2. Impedir suspensão do notebook

Abrir um terminal e rodar:

```bash
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-ac-timeout 0
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-battery-timeout 0
gsettings set org.gnome.desktop.screensaver lock-enabled false
gsettings set org.gnome.desktop.session idle-delay 0
```

---

### 3. Conectar celular via USB Tethering

1. Plugar o cabo USB no celular e no notebook
2. No celular: **Configurações → Rede → Ponto de acesso → Tethering USB** → ativar
3. No notebook, confirmar que a interface apareceu:

```bash
ip addr show usb0
```

Deve aparecer um IP na faixa `192.168.42.x`. O **celular sempre fica em `192.168.42.1`**.

---

### 4. Abrir o IP Webcam no celular

1. Abrir o app **IP Webcam**
2. Rolar até o final → **"Iniciar servidor"**
3. Anotar a porta (padrão: **8080**)
4. O stream RTSP do celular vai estar em: `rtsp://192.168.42.1:8080/video`

Testar no notebook se o stream abre:

```bash
ffplay rtsp://192.168.42.1:8080/video
```

Se abrir a imagem da câmera, está ok. Fechar o ffplay (`q`).

---

### 5. Atualizar o .env.netbook

Editar a linha do RTSP URL:

```bash
# Abrir o arquivo
nano .env.netbook
```

Alterar a linha:
```
REPLAY_RTSP_URL=rtsp://192.168.42.1:8080/video
```

Salvar (`Ctrl+O`, `Enter`, `Ctrl+X`).

> `REPLAY_SIMULATE=true` já está configurado — o gatilho será a tecla **Enter** no terminal, sem precisar de GPIO.

---

### 6. Buildar o sistema (ainda no desktop)

```bash
cd /home/edipo/Documents/SideProjects/replay-saas
go build -o replay-agent .
```

---

### 7. Mudar para terminal TTY (evita cliques acidentais)

Pressionar `Ctrl + Alt + F3` — a tela vai virar um terminal puro, sem desktop.

Fazer login com usuário e senha do Ubuntu.

```bash
cd /home/edipo/Documents/SideProjects/replay-saas
```

> Para voltar ao desktop quando quiser: `Ctrl + Alt + F1`

---

### 8. Rodar o sistema

```bash
env $(cat .env.netbook | grep -v '^#' | xargs) ./replay-agent
```

Você deve ver no log:
```
"msg":"video ingestion started"
[ simulate ] press Enter to trigger a replay (Ctrl+C to quit)
```

---

### 9. Testar antes do jogo começar

1. Apontar a câmera para o campo
2. Aguardar ~10 segundos (buffer precisa de alguns segmentos)
3. Pressionar **Enter** no terminal
4. Aguardar ~20-30 segundos
5. Verificar se o vídeo chegou no grupo do Telegram

---

## DURANTE O JOGO

- Pressionar **Enter** no terminal para gerar um replay
- O sistema loga tudo — se algo der errado, o erro aparece no terminal
- Não fechar o terminal nem deixar o notebook dormir

---

## APÓS O JOGO — Reverter configurações

```bash
gsettings reset org.gnome.settings-daemon.plugins.power sleep-inactive-ac-timeout
gsettings reset org.gnome.settings-daemon.plugins.power sleep-inactive-battery-timeout
gsettings set org.gnome.desktop.screensaver lock-enabled true
gsettings reset org.gnome.desktop.session idle-delay
```

Desativar o Tethering USB no celular.

---

## Diagnósticos rápidos

| Problema | O que checar |
|---|---|
| `ffplay` não abre o stream | IP Webcam iniciado? `ip addr show usb0` existe? |
| Log: `required env var not set` | `.env.netbook` salvo corretamente? |
| Replay não chega no Telegram | `agenda.json` tem o horário de hoje? Ver `/tmp/telegram.log` |
| `usb0` não aparece | Desativar e reativar Tethering USB no celular |
