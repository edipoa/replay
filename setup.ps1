$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

Write-Host ""
Write-Host "╔══════════════════════════════════════╗" -ForegroundColor Cyan
Write-Host "║      Replay Agent — Configuração     ║" -ForegroundColor Cyan
Write-Host "╚══════════════════════════════════════╝" -ForegroundColor Cyan
Write-Host ""

# Lê valor atual do .env (comentado ou não)
function Get-EnvVal([string]$key) {
    $line = Get-Content ".env" | Where-Object { $_ -match "^#?\s*$key=" } | Select-Object -First 1
    if ($line) { return ($line -split "=", 2)[1] }
    return $null
}

# Define (ou comenta, se valor vazio) uma chave no .env
function Set-EnvVal([string]$key, [string]$val) {
    $content = Get-Content ".env" -Raw
    if ([string]::IsNullOrWhiteSpace($val)) {
        $content = $content -replace "(?m)^$key=.*", "# $key="
    } else {
        # escapa $ no valor de substituição — -replace trata $1, $&, $$ etc como especiais
        $escVal = $val.Replace('$', '$$$$')
        $content = $content -replace "(?m)^#?\s*$key=.*", "$key=$escVal"
    }
    [System.IO.File]::WriteAllText((Resolve-Path ".env"), $content)
}

$defaultRtsp1 = Get-EnvVal "REPLAY_CAM_1_RTSP_URL"
if (-not $defaultRtsp1) { $defaultRtsp1 = "rtsp://192.168.42.1:8080/video" }
$defaultRtsp2 = Get-EnvVal "REPLAY_CAM_2_RTSP_URL"

Write-Host "1. URL da câmera 1 (RTSP)" -ForegroundColor Yellow
Write-Host "   Padrão IP Webcam via USB Tethering: rtsp://192.168.42.1:8080/video"
$rtspUrl1 = Read-Host "   URL [$defaultRtsp1]"
if ([string]::IsNullOrWhiteSpace($rtspUrl1)) { $rtspUrl1 = $defaultRtsp1 }

Write-Host ""
Write-Host "2. URL da câmera 2 (RTSP)" -ForegroundColor Yellow
Write-Host "   Opcional — deixe em branco para desabilitar"
$rtspUrl2 = Read-Host "   URL [$defaultRtsp2]"
if ([string]::IsNullOrWhiteSpace($rtspUrl2)) { $rtspUrl2 = $defaultRtsp2 }

Write-Host ""
Write-Host "3. Cloudflare R2 (upload na nuvem)" -ForegroundColor Yellow
Write-Host "   Opcional — deixe em branco para desabilitar"
$defaultAccountId = Get-EnvVal "REPLAY_R2_ACCOUNT_ID"
$r2AccountId = Read-Host "   Account ID [$defaultAccountId]"
if ([string]::IsNullOrWhiteSpace($r2AccountId)) { $r2AccountId = $defaultAccountId }
$defaultAccessKeyId = Get-EnvVal "REPLAY_R2_ACCESS_KEY_ID"
$r2AccessKeyId = Read-Host "   Access Key ID [$defaultAccessKeyId]"
if ([string]::IsNullOrWhiteSpace($r2AccessKeyId)) { $r2AccessKeyId = $defaultAccessKeyId }
$defaultSecretKey = Get-EnvVal "REPLAY_R2_SECRET_ACCESS_KEY"
$r2SecretKey = Read-Host "   Secret Access Key [$defaultSecretKey]"
if ([string]::IsNullOrWhiteSpace($r2SecretKey)) { $r2SecretKey = $defaultSecretKey }
$defaultBucket = Get-EnvVal "REPLAY_R2_BUCKET"
$r2Bucket = Read-Host "   Bucket [$defaultBucket]"
if ([string]::IsNullOrWhiteSpace($r2Bucket)) { $r2Bucket = $defaultBucket }

Write-Host ""
Write-Host "4. Backend (necessário só se R2 estiver habilitado)" -ForegroundColor Yellow
$defaultBackendUrl = Get-EnvVal "REPLAY_BACKEND_URL"
$backendUrl = Read-Host "   URL [$defaultBackendUrl]"
if ([string]::IsNullOrWhiteSpace($backendUrl)) { $backendUrl = $defaultBackendUrl }
$defaultBackendKey = Get-EnvVal "REPLAY_BACKEND_API_KEY"
$backendApiKey = Read-Host "   API Key [$defaultBackendKey]"
if ([string]::IsNullOrWhiteSpace($backendApiKey)) { $backendApiKey = $defaultBackendKey }

Set-EnvVal "REPLAY_CAM_1_RTSP_URL" $rtspUrl1
Set-EnvVal "REPLAY_CAM_2_RTSP_URL" $rtspUrl2
Set-EnvVal "REPLAY_CAM_2_BUFFER_DIR" $(if ($rtspUrl2) { "/tmp/replay_buffer_2" } else { "" })
Set-EnvVal "REPLAY_R2_ACCOUNT_ID" $r2AccountId
Set-EnvVal "REPLAY_R2_ACCESS_KEY_ID" $r2AccessKeyId
Set-EnvVal "REPLAY_R2_SECRET_ACCESS_KEY" $r2SecretKey
Set-EnvVal "REPLAY_R2_BUCKET" $r2Bucket
Set-EnvVal "REPLAY_BACKEND_URL" $backendUrl
Set-EnvVal "REPLAY_BACKEND_API_KEY" $backendApiKey

Write-Host ""
Write-Host "✓ .env salvo" -ForegroundColor Green

# Testa câmeras
function Test-Camera([string]$label, [string]$url) {
    Write-Host ""
    Write-Host "Testando $label..." -ForegroundColor Yellow
    & .\ffmpeg.exe -loglevel error -rtsp_transport tcp -i $url -t 2 -f null - 2>&1 | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "✓ $label OK" -ForegroundColor Green
    } else {
        Write-Host "✗ $label não respondeu. Verifique:" -ForegroundColor Red
        Write-Host "  • IP Webcam iniciado no celular (botão 'Iniciar servidor')?"
        Write-Host "  • Tethering USB ativo em Configurações → Ponto de acesso?"
        Write-Host "  • URL correta: $url"
    }
}

Test-Camera "câmera 1" $rtspUrl1
if ($rtspUrl2) { Test-Camera "câmera 2" $rtspUrl2 }

Write-Host ""
Write-Host "────────────────────────────────────────"
Write-Host "Iniciar sistema:  " -NoNewline; Write-Host "start.bat" -ForegroundColor Cyan
Write-Host ""
Read-Host "Pressione Enter para sair"
