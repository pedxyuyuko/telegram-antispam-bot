// Package telegram wraps go-telegram/bot and preserves updates before polling
// can advance its offset. Application work is drained from the durable inbox.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"telegram-antispam-bot/internal/config"
)

type DurableClient struct {
	HTTP    *http.Client
	Capture func(context.Context, []json.RawMessage) error
}

func (c *DurableClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(req.URL.Path, "/getUpdates") || resp.StatusCode != http.StatusOK {
		return resp, nil
	}
	const limit = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, errors.New("Telegram update batch exceeds 16 MiB")
	}
	var envelope struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode Telegram batch: %w", err)
	}
	if envelope.OK && len(envelope.Result) > 0 {
		if err := c.Capture(req.Context(), envelope.Result); err != nil {
			return nil, fmt.Errorf("persist Telegram batch: %w", err)
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}

type cachedProfile struct {
	value   Profile
	expires time.Time
}
type Client struct {
	Bot      *bot.Bot
	Me       *models.User
	token    string
	ttl      time.Duration
	mu       sync.Mutex
	profiles map[int64]cachedProfile
}

func New(cfg config.Telegram, token string, httpClient bot.HttpClient, lastID int64, log *slog.Logger) (*Client, error) {
	c := &Client{token: token, ttl: time.Duration(cfg.ProfileCacheSeconds) * time.Second, profiles: map[int64]cachedProfile{}}
	b, err := bot.New(token, bot.WithSkipGetMe(), bot.WithServerURL(cfg.APIBase),
		bot.WithHTTPClient(time.Duration(cfg.PollTimeoutSeconds)*time.Second, httpClient),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "edited_message"}), bot.WithInitialOffset(lastID),
		bot.WithWorkers(1), bot.WithNotAsyncHandlers(), bot.WithUpdatesChannelCap(1),
		bot.WithDefaultHandler(func(context.Context, *bot.Bot, *models.Update) {}),
		bot.WithErrorsHandler(func(err error) { log.Error("telegram polling", "error", c.safe(err)) }))
	if err != nil {
		return nil, err
	}
	c.Bot = b
	return c, nil
}

func (c *Client) safe(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "[redacted]"))
}
func (c *Client) Initialize(ctx context.Context) error {
	var err error
	c.Me, err = c.Bot.GetMe(ctx)
	if err != nil {
		return c.safe(err)
	}
	info, err := c.Bot.GetWebhookInfo(ctx)
	if err != nil {
		return c.safe(err)
	}
	if info.URL != "" {
		return errors.New("a Telegram webhook is configured; remove it before starting polling")
	}
	return nil
}
func (c *Client) Start(ctx context.Context) { c.Bot.Start(ctx) }

func (c *Client) Profile(ctx context.Context, m *Message, diagnostic *slog.Logger) Profile {
	id := m.UserID()
	if m.SenderChat != nil {
		id = m.SenderChat.ID
	}
	if id == 0 {
		diagnostic.Debug("profile.unavailable", "event", "profile.unavailable", "reason", "missing_sender")
		return Profile{Status: "unavailable"}
	}
	c.mu.Lock()
	cached, ok := c.profiles[id]
	c.mu.Unlock()
	if ok && time.Now().Before(cached.expires) {
		diagnostic.Debug("profile.cache_hit", "event", "profile.cache_hit", "sender", id, "status", cached.value.Status, "fetched_at", cached.value.FetchedAt)
		return cached.value
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	started := time.Now()
	diagnostic.Debug("profile.started", "event", "profile.started", "sender", id)
	chat, err := c.Bot.GetChat(ctx, &bot.GetChatParams{ChatID: id})
	p := Profile{Status: "available", FetchedAt: time.Now().Unix()}
	if err != nil {
		p.Status = "unavailable"
	} else {
		p.Bio = chat.Bio
		p.Description = chat.Description
	}
	diagnostic.Debug("profile.completed", "event", "profile.completed", "sender", id, "status", p.Status, "duration_ms", time.Since(started).Milliseconds(), "has_bio", p.Bio != "", "has_description", p.Description != "", "error", c.safe(err))
	c.mu.Lock()
	// Bound the ancillary cache; the archive and queue live in SQLite.
	if len(c.profiles) >= 10000 {
		for key, v := range c.profiles {
			if time.Now().After(v.expires) {
				delete(c.profiles, key)
			}
		}
		if len(c.profiles) >= 10000 {
			clear(c.profiles)
		}
	}
	c.profiles[id] = cachedProfile{p, time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return p
}

func (c *Client) MemberStatus(ctx context.Context, chat, user int64) (string, error) {
	m, err := c.Bot.GetChatMember(ctx, &bot.GetChatMemberParams{ChatID: chat, UserID: user})
	if err != nil {
		return "", c.safe(err)
	}
	if m.Type == models.ChatMemberTypeRestricted && m.Restricted != nil && !m.Restricted.IsMember {
		return "left", nil
	}
	return string(m.Type), nil
}
func (c *Client) Send(ctx context.Context, m *Message, text string) error {
	for len(text) > 0 {
		n := len(text)
		if n > 3500 {
			n = 3500
			for !utf8.RuneStart(text[n]) {
				n--
			}
		}
		_, err := c.Bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: m.Chat.ID, Text: text[:n], ReplyParameters: &models.ReplyParameters{MessageID: m.ID, AllowSendingWithoutReply: true}, DisableNotification: true})
		if err != nil {
			return c.safe(err)
		}
		text = text[n:]
	}
	return nil
}
func (c *Client) Delete(ctx context.Context, chat int64, message int) error {
	ok, err := c.Bot.DeleteMessage(ctx, &bot.DeleteMessageParams{ChatID: chat, MessageID: message})
	if err == nil && !ok {
		err = errors.New("Telegram declined deletion")
	}
	return c.safe(err)
}
func (c *Client) Mute(ctx context.Context, chat, user int64, until time.Time) error {
	remaining := time.Until(until)
	if remaining < 30*time.Second || remaining > 366*24*time.Hour {
		return errors.New("mute deadline would imply a permanent restriction")
	}
	ok, err := c.Bot.RestrictChatMember(ctx, &bot.RestrictChatMemberParams{ChatID: chat, UserID: user, Permissions: &models.ChatPermissions{}, UseIndependentChatPermissions: true, UntilDate: int(until.Unix())})
	if err == nil && !ok {
		err = errors.New("Telegram declined restriction")
	}
	return c.safe(err)
}
func (c *Client) Kick(ctx context.Context, chat, user int64) error {
	// Telegram's default unban operation removes a current member and permits
	// rejoining. One request also avoids an intermediate permanent-ban state.
	ok, err := c.Bot.UnbanChatMember(ctx, &bot.UnbanChatMemberParams{ChatID: chat, UserID: user, OnlyIfBanned: false})
	if err == nil && !ok {
		err = errors.New("Telegram declined removal")
	}
	return c.safe(err)
}
