BINARY  := replay-agent
TOOLS   := tools
DIST    := dist

# FFmpeg static binaries — download once and place in tools/:
#   Linux:   https://johnvansickle.com/ffmpeg/  → tools/ffmpeg-linux
#   Windows: https://www.gyan.dev/ffmpeg/builds/ → tools/ffmpeg-windows.exe
#
# Música de fundo (opcional) — coloque em tools/music.mp3 (ou .aac/.wav).
# Se existir, será copiada para o dist e o .env será descomentado automaticamente.
MUSIC_FILE := $(wildcard $(TOOLS)/music.mp3 $(TOOLS)/music.aac $(TOOLS)/music.wav)

.PHONY: dist dist-linux dist-windows clean seed seed-dry

dist: dist-linux dist-windows

dist-linux: _check-ffmpeg-linux
	@echo "→ building linux/amd64"
	@mkdir -p $(DIST)/linux
	GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o $(DIST)/linux/$(BINARY) .
	cp $(TOOLS)/ffmpeg-linux          $(DIST)/linux/ffmpeg
	cp faz-o-clip-watermark-220.png   $(DIST)/linux/watermark.png
	cp agenda.json                    $(DIST)/linux/agenda.json
	cp .env.example                   $(DIST)/linux/.env
	cp start.sh                       $(DIST)/linux/start.sh
	cp setup.sh                       $(DIST)/linux/setup.sh
	$(if $(MUSIC_FILE),cp $(MUSIC_FILE) $(DIST)/linux/music$(suffix $(MUSIC_FILE)) && \
		sed -i 's|^# REPLAY_MUSIC_PATH=.*|REPLAY_MUSIC_PATH=music$(suffix $(MUSIC_FILE))|' $(DIST)/linux/.env && \
		echo "🎵  Música incluída: music$(suffix $(MUSIC_FILE))")
	chmod +x $(DIST)/linux/$(BINARY) $(DIST)/linux/start.sh $(DIST)/linux/setup.sh $(DIST)/linux/ffmpeg
	cd $(DIST) && zip -r $(BINARY)-linux.zip linux/
	@echo ""
	@echo "✅  $(DIST)/$(BINARY)-linux.zip"
	@echo "ℹ️   Cliente roda setup.sh para configurar — .env não precisa ser editado manualmente."

dist-windows: _check-ffmpeg-windows
	@echo "→ building windows/amd64"
	@mkdir -p $(DIST)/windows
	GOOS=windows GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o $(DIST)/windows/$(BINARY).exe .
	cp $(TOOLS)/ffmpeg-windows.exe    $(DIST)/windows/ffmpeg.exe
	cp faz-o-clip-watermark-220.png   $(DIST)/windows/watermark.png
	cp agenda.json                    $(DIST)/windows/agenda.json
	cp .env.example                   $(DIST)/windows/.env
	cp start.bat                      $(DIST)/windows/start.bat
	cp setup.ps1                      $(DIST)/windows/setup.ps1
	$(if $(MUSIC_FILE),cp $(MUSIC_FILE) $(DIST)/windows/music$(suffix $(MUSIC_FILE)) && \
		sed -i 's|^# REPLAY_MUSIC_PATH=.*|REPLAY_MUSIC_PATH=music$(suffix $(MUSIC_FILE))|' $(DIST)/windows/.env && \
		echo "🎵  Música incluída: music$(suffix $(MUSIC_FILE))")
	cd $(DIST) && zip -r $(BINARY)-windows.zip windows/
	@echo ""
	@echo "✅  $(DIST)/$(BINARY)-windows.zip"
	@echo "ℹ️   Cliente roda setup.ps1 para configurar — .env não precisa ser editado manualmente."

seed: ## Insere vídeos de simulação no backend (R2 + PHP). Ex: make seed N=5 DAYS=30 CAM=cam1,cam2
	go run ./cmd/seed/ -n $(or $(N),5) -days $(or $(DAYS),30) -cam $(or $(CAM),cam1,cam2) -duration $(or $(DUR),30)

seed-dry: ## Dry-run: só gera os vídeos, sem upload
	go run ./cmd/seed/ -n $(or $(N),3) -days $(or $(DAYS),30) -cam $(or $(CAM),cam1,cam2) -duration $(or $(DUR),5) -dry-run

clean:
	rm -rf $(DIST)/

_check-ffmpeg-linux:
	@test -f $(TOOLS)/ffmpeg-linux || \
		(echo "❌ Baixe o ffmpeg estático para Linux em https://johnvansickle.com/ffmpeg/" && \
		 echo "   e salve em $(TOOLS)/ffmpeg-linux" && exit 1)

_check-ffmpeg-windows:
	@test -f $(TOOLS)/ffmpeg-windows.exe || \
		(echo "❌ Baixe o ffmpeg para Windows em https://www.gyan.dev/ffmpeg/builds/" && \
		 echo "   e salve em $(TOOLS)/ffmpeg-windows.exe" && exit 1)
