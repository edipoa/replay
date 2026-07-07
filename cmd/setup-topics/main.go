// setup-topics cria tópicos no fórum do Telegram para cada slot do agenda.json
// e atualiza o arquivo com os thread_ids reais.
//
// Uso:
//
//	go run ./cmd/setup-topics/             # cria apenas slots com thread_id == 0
//	go run ./cmd/setup-topics/ -force      # recria todos os slots (ignora IDs existentes)
//
// Variáveis de ambiente (ou .env.netbook na raiz do projeto):
//
//	REPLAY_BOT_TOKEN   token do bot Telegram
//	REPLAY_CHAT_ID     ID do grupo (inteiro negativo, ex: -1001234567890)
//	REPLAY_AGENDA_PATH caminho para o agenda.json (padrão: ./agenda.json)
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/joho/godotenv"

	"github.com/edipo/replay-saas/internal/envutil"
)

func main() {
	force := flag.Bool("force", false, "recria tópicos mesmo para slots já configurados (thread_id != 0)")
	flag.Parse()

	// Tenta carregar .env.netbook ou .env da raiz do projeto.
	_ = godotenv.Load(".env.netbook")
	_ = godotenv.Load(".env")

	token := envutil.MustEnv("REPLAY_BOT_TOKEN")
	chatID := envutil.MustEnv("REPLAY_CHAT_ID")
	agendaPath := envutil.Or("REPLAY_AGENDA_PATH", "agenda.json")

	data, err := os.ReadFile(agendaPath)
	if err != nil {
		log.Fatalf("ler agenda: %v", err)
	}

	var agenda map[string]int64
	if err := json.Unmarshal(data, &agenda); err != nil {
		log.Fatalf("parsear agenda: %v", err)
	}

	keys := sortedKeys(agenda)
	client := &http.Client{Timeout: 10 * time.Second}

	created, skipped, failed := 0, 0, 0

	for _, key := range keys {
		existing := agenda[key]
		if existing != 0 && !*force {
			fmt.Printf("⏭  skip  %-25s (thread_id=%d)\n", key, existing)
			skipped++
			continue
		}

		threadID, err := createTopic(client, token, chatID, key)
		if err != nil {
			fmt.Printf("❌ erro  %-25s %v\n", key, err)
			failed++
			continue
		}

		agenda[key] = threadID
		fmt.Printf("✅ criado %-25s => %d\n", key, threadID)
		created++

		time.Sleep(2 * time.Second) // evitar rate-limit do Telegram
	}

	out, err := json.MarshalIndent(agenda, "", "  ")
	if err != nil {
		log.Fatalf("serializar agenda: %v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(agendaPath, out, 0o644); err != nil {
		log.Fatalf("salvar agenda: %v", err)
	}

	fmt.Printf("\ncriados: %d | ignorados: %d | erros: %d\nagenda salva em %s\n",
		created, skipped, failed, agendaPath)
}

// ─── Telegram API ─────────────────────────────────────────────────────────────

type createTopicReq struct {
	ChatID string `json:"chat_id"`
	Name   string `json:"name"`
}

type createTopicResp struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageThreadID int64 `json:"message_thread_id"`
	} `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// createTopic cria um tópico no fórum e respeita o Retry-After em caso de 429.
func createTopic(client *http.Client, token, chatID, name string) (int64, error) {
	for {
		body, _ := json.Marshal(createTopicReq{ChatID: chatID, Name: name})

		resp, err := client.Post(
			fmt.Sprintf("https://api.telegram.org/bot%s/createForumTopic", token),
			"application/json",
			bytes.NewReader(body),
		)
		if err != nil {
			return 0, fmt.Errorf("http: %w", err)
		}

		var result createTopicResp
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			resp.Body.Close()
			return 0, fmt.Errorf("decode: %w", err)
		}
		resp.Body.Close()

		if result.OK {
			return result.Result.MessageThreadID, nil
		}

		// Rate limited — aguarda o tempo indicado pelo Telegram e tenta de novo.
		if result.ErrorCode == 429 && result.Parameters != nil && result.Parameters.RetryAfter > 0 {
			wait := time.Duration(result.Parameters.RetryAfter+1) * time.Second
			fmt.Printf("   ⏳ rate limit em %q, aguardando %v...\n", name, wait)
			time.Sleep(wait)
			continue
		}

		return 0, fmt.Errorf("telegram: %s", result.Description)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

