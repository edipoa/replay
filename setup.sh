#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'

echo ""
echo -e "${BLUE}╔══════════════════════════════════════╗${NC}"
echo -e "${BLUE}║      Replay Agent — Configuração     ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════╝${NC}"
echo ""

# Carrega valores atuais do .env se existir
_get() { grep -m1 "^$1=" .env 2>/dev/null | cut -d= -f2- || true; }

DEFAULT_RTSP="$(_get REPLAY_CAM_1_RTSP_URL)"
DEFAULT_RTSP="${DEFAULT_RTSP:-rtsp://192.168.42.1:8080/video}"

echo -e "${YELLOW}1. URL da câmera (RTSP)${NC}"
echo "   Padrão IP Webcam via USB Tethering: rtsp://192.168.42.1:8080/video"
read -rp "   URL [$DEFAULT_RTSP]: " RTSP_URL
RTSP_URL="${RTSP_URL:-$DEFAULT_RTSP}"

echo ""
echo -e "${YELLOW}2. Token do bot Telegram${NC}"
echo "   Gerado pelo @BotFather — formato: 1234567890:ABC..."
read -rp "   Token: " BOT_TOKEN

echo ""
echo -e "${YELLOW}3. ID do grupo Telegram${NC}"
echo "   Número negativo, ex: -1001234567890"
read -rp "   Chat ID: " CHAT_ID

# Atualiza apenas as 3 linhas no .env existente
sed -i \
    -e "s|^REPLAY_CAM_1_RTSP_URL=.*|REPLAY_CAM_1_RTSP_URL=$RTSP_URL|" \
    -e "s|^REPLAY_BOT_TOKEN=.*|REPLAY_BOT_TOKEN=$BOT_TOKEN|" \
    -e "s|^REPLAY_CHAT_ID=.*|REPLAY_CHAT_ID=$CHAT_ID|" \
    .env

echo ""
echo -e "${GREEN}✓ .env salvo${NC}"

# Testa câmera
echo ""
echo -e "${YELLOW}Testando câmera...${NC}"
if ./ffmpeg -loglevel error -rtsp_transport tcp -i "$RTSP_URL" -t 2 -f null - 2>/dev/null; then
    echo -e "${GREEN}✓ Câmera OK${NC}"
else
    echo -e "${RED}✗ Câmera não respondeu. Verifique:${NC}"
    echo "  • IP Webcam iniciado no celular (botão 'Iniciar servidor')?"
    echo "  • Tethering USB ativo em Configurações → Ponto de acesso?"
    echo "  • URL correta: $RTSP_URL"
    echo "  Teste manual: ./ffmpeg -i \"$RTSP_URL\" -t 2 -f null -"
fi

# Testa Telegram
echo ""
echo -e "${YELLOW}Testando bot Telegram...${NC}"
TG=$(curl -sf "https://api.telegram.org/bot${BOT_TOKEN}/getMe" 2>/dev/null || true)
if echo "$TG" | grep -q '"ok":true'; then
    BOT_NAME=$(echo "$TG" | grep -o '"username":"[^"]*"' | cut -d'"' -f4)
    echo -e "${GREEN}✓ Bot @${BOT_NAME} conectado${NC}"
else
    echo -e "${RED}✗ Token inválido ou sem conexão. Verifique REPLAY_BOT_TOKEN${NC}"
fi

echo ""
echo "────────────────────────────────────────"
echo -e "Iniciar sistema:  ${BLUE}./start.sh${NC}"
echo -e "Modo teste (Enter no teclado): ${BLUE}REPLAY_SIMULATE=true ./start.sh${NC}"
echo ""
