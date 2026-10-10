// control: the Telegram bot (ported from Engine/lossguard) that watches a Binance USD-M futures
// account every 10 seconds, alerts at a 10% daily loss and closes everything + blocks trading
// for 24h at 20%. Secrets come from the environment (Railway variables); .env is local-only.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // IANA zones on Windows

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"control/internal/bot"
	"control/internal/config"
	"control/internal/guard"
	"control/internal/runners"
	"control/internal/store"
)

func main() {
	envFile := flag.String("env", ".env", "optional local .env file (ignored when absent)")
	checkOnly := flag.Bool("check", false, "validate config and Telegram token, then exit")
	flag.Parse()

	cfg, err := config.Load(*envFile)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	var api *tgbotapi.BotAPI
	for attempt := 1; ; attempt++ { // ride out short network outages at startup
		if api, err = tgbotapi.NewBotAPI(cfg.BotToken); err == nil {
			break
		}
		if attempt == 5 {
			log.Fatalf("telegram: %v", err)
		}
		log.Printf("telegram: %v (retry %d/5)", err, attempt)
		time.Sleep(time.Duration(attempt) * 3 * time.Second)
	}
	log.Printf("telegram: authorized as @%s", api.Self.UserName)
	if *checkOnly {
		return
	}

	st, err := store.Open(cfg.StateFile, cfg.EncryptionKey)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	b := bot.New(api, st, bot.Options{
		AllowedUserIDs:   cfg.AllowedUserIDs,
		AllowedUsernames: cfg.AllowedUsernames,
		DefaultLang:      cfg.DefaultLang,
		PresetKey:        cfg.PresetKey,
		PresetSecret:     cfg.PresetSecret,
	})
	if !cfg.HasAllowlist() {
		log.Print("WARNING: ALLOWED_USER_IDS is empty - anyone who finds the bot can use it")
	}
	if cfg.PresetKey != "" {
		log.Print("binance: using preset API keys from the environment for /start")
	}
	g := guard.New(guard.Options{
		FapiURL:  cfg.FapiURL,
		SapiURL:  cfg.SapiURL,
		WsURL:    cfg.WsURL,
		Interval: cfg.CheckInterval,
		Location: cfg.Location,
		Rules: guard.Rules{
			AlertPct:        cfg.AlertLossPct,
			BlockPct:        cfg.BlockLossPct,
			BlockFor:        cfg.BlockDuration,
			Period:          cfg.Period,
			AlertResetAfter: cfg.AlertResetAfter,
		},
	}, st, b)
	b.SetGuard(g)
	b.RegisterCommands()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(cfg.Runners) > 0 {
		clients := make([]*runners.Client, 0, len(cfg.Runners))
		for _, r := range cfg.Runners {
			clients = append(clients, runners.NewClient(r.Name, r.URL, cfg.ControlToken))
		}
		reg := runners.NewRegistry(clients, cfg.RunnerPollInterval, cfg.DefaultLang, b)
		b.SetRunners(reg)
		go reg.Run(ctx)
		log.Printf("runners: %d configured, poll every %s", len(clients), cfg.RunnerPollInterval)
	}
	go g.Run(ctx)
	log.Printf("control: checking %d active account(s) every %s (alert %.0f%%, block %.0f%% for %s)",
		len(st.Active()), cfg.CheckInterval, cfg.AlertLossPct, cfg.BlockLossPct, cfg.BlockDuration)
	b.Run(ctx)
	log.Print("control: stopped")
}
