#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
touch .env

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BLUE='\033[0;34m'; NC='\033[0m'

echo ""
echo -e "${BLUE}╔══════════════════════════════════════╗${NC}"
echo -e "${BLUE}║      Replay Agent — Configuração     ║${NC}"
echo -e "${BLUE}╚══════════════════════════════════════╝${NC}"
echo ""

# Carrega valor atual do .env (comentado ou não) se existir
_get() { grep -m1 "^#\? *$1=" .env 2>/dev/null | cut -d= -f2- || true; }

# Define (ou comenta, se valor vazio) uma chave no .env
_set() {
    local key="$1" val="$2"
    if [ -n "$val" ]; then
        # escapa & \ e | (delimitador do sed) para não virarem código no replacement
        local esc_val; esc_val="$(printf '%s' "$val" | sed -e 's/[\&|]/\\&/g')"
        if grep -q "^$key=" .env; then
            sed -i "s|^$key=.*|$key=$esc_val|" .env
        elif grep -q "^#\? *$key=" .env; then
            sed -i "s|^#\? *$key=.*|$key=$esc_val|" .env
        else
            echo "$key=$esc_val" >> .env
        fi
    else
        if grep -q "^$key=" .env; then
            sed -i "s|^$key=.*|# $key=|" .env
        fi
    fi
}

DEFAULT_RTSP_1="$(_get REPLAY_CAM_1_RTSP_URL)"
DEFAULT_RTSP_1="${DEFAULT_RTSP_1:-rtsp://192.168.42.1:8080/video}"
DEFAULT_RTSP_2="$(_get REPLAY_CAM_2_RTSP_URL)"

echo -e "${YELLOW}1. URL da câmera 1 (RTSP)${NC}"
echo "   Padrão IP Webcam via USB Tethering: rtsp://192.168.42.1:8080/video"
read -rp "   URL [$DEFAULT_RTSP_1]: " RTSP_URL_1
RTSP_URL_1="${RTSP_URL_1:-$DEFAULT_RTSP_1}"

echo ""
echo -e "${YELLOW}2. URL da câmera 2 (RTSP)${NC}"
echo "   Opcional — deixe em branco para desabilitar"
read -rp "   URL [$DEFAULT_RTSP_2]: " RTSP_URL_2
RTSP_URL_2="${RTSP_URL_2:-$DEFAULT_RTSP_2}"

echo ""
echo -e "${YELLOW}3. Cloudflare R2 (upload na nuvem)${NC}"
echo "   Opcional — deixe em branco para desabilitar"
read -rp "   Account ID [$(_get REPLAY_R2_ACCOUNT_ID)]: " R2_ACCOUNT_ID
R2_ACCOUNT_ID="${R2_ACCOUNT_ID:-$(_get REPLAY_R2_ACCOUNT_ID)}"
read -rp "   Access Key ID [$(_get REPLAY_R2_ACCESS_KEY_ID)]: " R2_ACCESS_KEY_ID
R2_ACCESS_KEY_ID="${R2_ACCESS_KEY_ID:-$(_get REPLAY_R2_ACCESS_KEY_ID)}"
read -rp "   Secret Access Key [$(_get REPLAY_R2_SECRET_ACCESS_KEY)]: " R2_SECRET_ACCESS_KEY
R2_SECRET_ACCESS_KEY="${R2_SECRET_ACCESS_KEY:-$(_get REPLAY_R2_SECRET_ACCESS_KEY)}"
read -rp "   Bucket [$(_get REPLAY_R2_BUCKET)]: " R2_BUCKET
R2_BUCKET="${R2_BUCKET:-$(_get REPLAY_R2_BUCKET)}"

echo ""
echo -e "${YELLOW}4. Backend (necessário só se R2 estiver habilitado)${NC}"
DEFAULT_BACKEND_URL="$(_get REPLAY_BACKEND_URL)"
read -rp "   URL [$DEFAULT_BACKEND_URL]: " BACKEND_URL
BACKEND_URL="${BACKEND_URL:-$DEFAULT_BACKEND_URL}"
read -rp "   API Key [$(_get REPLAY_BACKEND_API_KEY)]: " BACKEND_API_KEY
BACKEND_API_KEY="${BACKEND_API_KEY:-$(_get REPLAY_BACKEND_API_KEY)}"

_set REPLAY_CAM_1_RTSP_URL "$RTSP_URL_1"
_set REPLAY_CAM_2_RTSP_URL "$RTSP_URL_2"
_set REPLAY_CAM_2_BUFFER_DIR "$([ -n "$RTSP_URL_2" ] && echo "/tmp/replay_buffer_2")"
_set REPLAY_R2_ACCOUNT_ID "$R2_ACCOUNT_ID"
_set REPLAY_R2_ACCESS_KEY_ID "$R2_ACCESS_KEY_ID"
_set REPLAY_R2_SECRET_ACCESS_KEY "$R2_SECRET_ACCESS_KEY"
_set REPLAY_R2_BUCKET "$R2_BUCKET"
_set REPLAY_BACKEND_URL "$BACKEND_URL"
_set REPLAY_BACKEND_API_KEY "$BACKEND_API_KEY"

echo ""
echo -e "${GREEN}✓ .env salvo${NC}"

# Testa câmeras
test_camera() {
    local label="$1" url="$2"
    echo ""
    echo -e "${YELLOW}Testando $label...${NC}"
    if ./ffmpeg -loglevel error -rtsp_transport tcp -i "$url" -t 2 -f null - 2>/dev/null; then
        echo -e "${GREEN}✓ $label OK${NC}"
    else
        echo -e "${RED}✗ $label não respondeu. Verifique:${NC}"
        echo "  • IP Webcam iniciado no celular (botão 'Iniciar servidor')?"
        echo "  • Tethering USB ativo em Configurações → Ponto de acesso?"
        echo "  • URL correta: $url"
        echo "  Teste manual: ./ffmpeg -i \"$url\" -t 2 -f null -"
    fi
}

test_camera "câmera 1" "$RTSP_URL_1"
[ -n "$RTSP_URL_2" ] && test_camera "câmera 2" "$RTSP_URL_2"

echo ""
echo "────────────────────────────────────────"
echo -e "Iniciar sistema:  ${BLUE}./start.sh${NC}"
echo -e "Modo teste (Enter no teclado): ${BLUE}REPLAY_SIMULATE=true ./start.sh${NC}"
echo ""
