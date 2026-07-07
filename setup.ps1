$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

Write-Host ""
Write-Host "╔══════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║      Replay Agent — Configuração     ║" -ForegroundColor Cyan
Write-Host "╚══════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# Lê valor atual do .env
function Get-EnvVal([string]$key) {
    $line = Get-Content ".env" | Where-Object { $_ -match "^$key=" } | Select-Object -First 1
    if ($line) { return ($line -split "=", 2)[1] }
    return $null
}

$defaultRtsp = Get-EnvVal "REPLAY_CAM_1_RTSP_URL"
if (-not $defaultRtsp) { $defaultRtsp = "rtsp://192.168.42.1:8080/video" }

Write-Host "1. URL da câmera (RTSP)" -ForegroundColor Yellow
Write-Host "   Padrão IP Webcam via USB Tethering: rtsp://192.168.42.1:8080/video"
$rtspUrl = Read-Host "   URL [$defaultRtsp]"
if ([string]::IsNullOrWhiteSpace($rtspUrl)) { $rtspUrl = $defaultRtsp }

Write-Host ""
Write-Host "2. Token do bot Telegram" -ForegroundColor Yellow
Write-Host "   Gerado pelo @BotFather — formato: 1234567890:ABC..."
$botToken = Read-Host "   Token"

Write-Host ""
Write-Host "3. ID do grupo Telegram" -ForegroundColor Yellow
Write-Host "   Número negativo, ex: -1001234567890"
$chatId = Read-Host "   Chat ID"

# Atualiza .env (apenas as 3 linhas)
$content = Get-Content ".env" -Raw
$content = $content -replace '(?m)^REPLAY_CAM_1_RTSP_URL=.*', "REPLAY_CAM_1_RTSP_URL=$rtspUrl"
$content = $content -replace '(?m)^REPLAY_BOT_TOKEN=.*',      "REPLAY_BOT_TOKEN=$botToken"
$content = $content -replace '(?m)^REPLAY_CHAT_ID=.*',        "REPLAY_CHAT_ID=$chatId"
[System.IO.File]::WriteAllText((Resolve-Path ".env"), $content)

Write-Host ""
Write-Host "✓ .env salvo" -ForegroundColor Green

# Testa câmera
Write-Host ""
Write-Host "Testando câmera..." -ForegroundColor Yellow
& .\ffmpeg.exe -loglevel error -rtsp_transport tcp -i $rtspUrl -t 2 -f null - 2>&1 | Out-Null
if ($LASTEXITCODE -eq 0) {
    Write-Host "✓ Câmera OK" -ForegroundColor Green
} else {
    Write-Host "✗ Câmera não respondeu. Verifique:" -ForegroundColor Red
    Write-Host "  • IP Webcam iniciado no celular (botão 'Iniciar servidor')?"
    Write-Host "  • Tethering USB ativo em Configurações → Ponto de acesso?"
    Write-Host "  • URL correta: $rtspUrl"
}

# Testa Telegram
Write-Host ""
Write-Host "Testando bot Telegram..." -ForegroundColor Yellow
try {
    $tg = Invoke-RestMethod "https://api.telegram.org/bot${botToken}/getMe"
    Write-Host "✓ Bot @$($tg.result.username) conectado" -ForegroundColor Green
} catch {
    Write-Host "✗ Token inválido ou sem conexão. Verifique REPLAY_BOT_TOKEN" -ForegroundColor Red
}

Write-Host ""
Write-Host "────────────────────────────────────────"
Write-Host "Iniciar sistema:  " -NoNewline; Write-Host "start.bat" -ForegroundColor Cyan
Write-Host ""
Read-Host "Pressione Enter para sair"
