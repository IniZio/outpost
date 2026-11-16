package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/IniZio/outpost/internal/domain"
)

// config is the server's settings, all from the environment. Port 7400 keeps clear of the
// prototype's 7300.
type config struct {
	DatabaseURL string         // OUTPOST_DATABASE_URL, required
	Listen      string         // OUTPOST_LISTEN
	PublicURL   string         // OUTPOST_PUBLIC_URL: where people reach the server
	AgentURL    string         // OUTPOST_AGENT_URL: the URL agents dial; its host is bound into every proof
	SecretKey   string         // OUTPOST_SECRET_KEY: base64 of 32 bytes; `outpost key` prints one
	BaseImage   string         // OUTPOST_BASE_IMAGE, required: the image a box starts from
	MaxRunning  int            // OUTPOST_MAX_RUNNING
	Allowed     []domain.Login // OUTPOST_ALLOWED_LOGINS: who may sign in with a GitHub token
	ClaudeModel string         // OUTPOST_CLAUDE_MODEL
	ClaudeBin   string         // OUTPOST_CLAUDE_BIN: the agent's command in the box
	Migrate     bool           // OUTPOST_MIGRATE: migrate at start
	ProxyURL    string         // OUTPOST_PROXY_URL: the credential proxy boxes dial; empty runs boxes with no credentials
	ProxyCAFile string         // OUTPOST_PROXY_CA_FILE: the proxy's public CA
	ProxyToken  string         // OUTPOST_PROXY_TOKEN: the bearer the proxy presents for grants
	Drain       time.Duration  // OUTPOST_DRAIN: budget for the hub drain and the HTTP shutdown
	LogLevel    slog.Level     // OUTPOST_LOG
}

func loadConfig(env func(string) string) (config, error) {
	get := func(k, def string) string {
		if v := env(k); v != "" {
			return v
		}
		return def
	}
	c := config{
		DatabaseURL: env("OUTPOST_DATABASE_URL"),
		Listen:      get("OUTPOST_LISTEN", ":7400"),
		PublicURL:   strings.TrimRight(get("OUTPOST_PUBLIC_URL", "http://localhost:7400"), "/"),
		AgentURL:    get("OUTPOST_AGENT_URL", "ws://127.0.0.1:7400/machines/link"),
		SecretKey:   env("OUTPOST_SECRET_KEY"),
		BaseImage:   env("OUTPOST_BASE_IMAGE"),
		ClaudeModel: env("OUTPOST_CLAUDE_MODEL"),
		ClaudeBin:   env("OUTPOST_CLAUDE_BIN"),
		Migrate:     get("OUTPOST_MIGRATE", "true") == "true",
		ProxyURL:    env("OUTPOST_PROXY_URL"),
		ProxyCAFile: env("OUTPOST_PROXY_CA_FILE"),
		ProxyToken:  env("OUTPOST_PROXY_TOKEN"),
	}
	c.Allowed = logins(env("OUTPOST_ALLOWED_LOGINS"))
	errs := c.parse(get)
	if u, err := url.Parse(c.AgentURL); err != nil || u.Host == "" {
		errs = append(errs, errors.New("OUTPOST_AGENT_URL must be a ws:// or wss:// URL"))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("OUTPOST_DATABASE_URL is required"))
	}
	return c, errors.Join(errs...)
}

func logins(list string) []domain.Login {
	var out []domain.Login
	for _, l := range strings.Split(list, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, domain.Login(l))
		}
	}
	return out
}

// parse reads the settings that are not plain strings.
func (c *config) parse(get func(k, def string) string) []error {
	var errs []error
	var err error
	if c.MaxRunning, err = strconv.Atoi(get("OUTPOST_MAX_RUNNING", "2")); err != nil || c.MaxRunning < 1 {
		errs = append(errs, errors.New("OUTPOST_MAX_RUNNING must be a positive number"))
	}
	if c.Drain, err = time.ParseDuration(get("OUTPOST_DRAIN", "60s")); err != nil {
		errs = append(errs, fmt.Errorf("OUTPOST_DRAIN: %w", err))
	}
	if err := c.LogLevel.UnmarshalText([]byte(get("OUTPOST_LOG", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("OUTPOST_LOG: %w", err))
	}
	return errs
}

// serveReady adds the settings only serve needs.
func (c config) serveReady() error {
	var errs []error
	if c.SecretKey == "" {
		errs = append(errs, errors.New("OUTPOST_SECRET_KEY is required; `outpost key` prints a new one"))
	}
	if c.BaseImage == "" {
		errs = append(errs, errors.New("OUTPOST_BASE_IMAGE is required: the image every box starts from"))
	}
	return errors.Join(errs...)
}
