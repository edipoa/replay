package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Poller fetches GET /api/live/state from the replay-site backend on an interval
// and pushes each result into Gate. A failed poll leaves the gate untouched
// (last-known-state wins).
type Poller struct {
	URL      string        // e.g. https://api.seusite.com.br/api/live/state
	APIKey   string        // sent as X-Api-Key
	Interval time.Duration
	Client   *http.Client
	Gate     *Gate
	Logger   *slog.Logger

	// OnPollError, if set, is called on every failed poll with the running
	// failure streak — used by main to emit an obs event without this package
	// importing obs.
	OnPollError func(err error, streak int)
}

// Run polls until ctx is cancelled. The first poll fires immediately.
func (p *Poller) Run(ctx context.Context) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	timer := time.NewTimer(0)
	defer timer.Stop()

	streak := 0
	first := true
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		st, err := fetchState(ctx, client, p.URL, p.APIKey)
		switch {
		case err != nil:
			streak++
			// Rate-limit the noise: first failure, then every 10th.
			if streak == 1 || streak%10 == 0 {
				p.Logger.Warn("live: poll do estado falhou — mantendo último estado",
					slog.Int("streak", streak), slog.Any("error", err))
			}
			if p.OnPollError != nil {
				p.OnPollError(err, streak)
			}
		default:
			if streak > 0 {
				p.Logger.Info("live: poll voltou a responder", slog.Int("after_failures", streak))
			}
			streak = 0
			changed := first || st.On != p.Gate.IsOn()
			p.Gate.Set(st)
			if changed {
				p.Logger.Info("live: estado atualizado",
					slog.Bool("on", st.On), slog.String("reason", st.Reason), slog.String("mode", st.Mode))
			}
			first = false
		}

		timer.Reset(p.Interval)
	}
}

func fetchState(ctx context.Context, client *http.Client, url, apiKey string) (State, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return State{}, err
	}
	if apiKey != "" {
		req.Header.Set("X-Api-Key", apiKey)
	}

	resp, err := client.Do(req)
	if err != nil {
		return State{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return State{}, fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}

	var st State
	if err := json.Unmarshal(body, &st); err != nil {
		return State{}, fmt.Errorf("decode: %w", err)
	}
	return st, nil
}
