package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"telegram-antispam-bot/examples"
	"telegram-antispam-bot/internal/app"
	"telegram-antispam-bot/internal/config"
	"telegram-antispam-bot/internal/decision"
	"telegram-antispam-bot/internal/store"
	"telegram-antispam-bot/internal/telegram"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "antispam:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, logs io.Writer) (err error) {
	flags := flag.NewFlagSet("antispam", flag.ContinueOnError)
	flags.SetOutput(logs)
	path := flags.String("config", "config.json", "configuration file")
	check := flags.Bool("check", false, "validate configuration without network access or opening the database")
	example := flags.Bool("example", false, "print an example configuration")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *example {
		c, err := config.Decode([]byte(examples.Scoring()))
		if err != nil {
			return fmt.Errorf("embedded example: %w", err)
		}
		e := json.NewEncoder(out)
		e.SetIndent("", "  ")
		return e.Encode(c)
	}
	m, err := config.Open(*path)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	c := m.Snapshot()
	if *check {
		_, err := fmt.Fprintf(out, "configuration valid: base mode=%s, group overrides=%d\n", c.Base.Mode, len(c.Groups))
		return err
	}
	token := strings.TrimSpace(os.Getenv(c.Telegram.TokenEnv))
	if token == "" {
		return fmt.Errorf("environment variable %s is required", c.Telegram.TokenEnv)
	}
	absConfig, err := filepath.Abs(*path)
	if err != nil {
		return err
	}
	dbpath := c.Database
	if !filepath.IsAbs(dbpath) {
		dbpath = filepath.Join(filepath.Dir(absConfig), dbpath)
	}
	s, err := store.Open(dbpath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	if err := s.Resize(ctx, c); err != nil {
		return err
	}
	lastID, err := s.LastUpdateID(ctx)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	telegramHTTP := &http.Client{Timeout: time.Duration(c.Telegram.PollTimeoutSeconds+10) * time.Second, CheckRedirect: noRedirect}
	durable := &telegram.DurableClient{HTTP: telegramHTTP, Capture: s.Capture}
	tg, err := telegram.New(c.Telegram, token, durable, lastID, log)
	if err != nil {
		return err
	}
	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = tg.Initialize(initCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("Telegram initialization: %w", err)
	}
	if tg.Me.Username == "" {
		return errors.New("Telegram bot has no username")
	}
	log.Info("starting antispam", "bot", tg.Me.Username, "base_mode", c.Base.Mode, "database", dbpath, "last_update_id", lastID, "model", c.Model.Name)
	a := &app.App{Config: m, Store: s, Telegram: tg, Model: &decision.Client{HTTP: &http.Client{CheckRedirect: noRedirect}}, Log: log}
	return a.Run(ctx)
}
