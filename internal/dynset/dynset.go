// Package dynset ports modules/Global/dynamic_settings.py: runtime-changeable
// settings (the AI url and session id) that persist to dynamic_settings.json and
// fall back to the static config when unset.
package dynset

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// Settings manages the dynamic settings, mirroring the Python DynamicSettings
// singleton. It is safe for concurrent use (the admin command writes while the
// AI worker reads).
type Settings struct {
	path            string
	defaultURL      string
	defaultSession  string
	defaultDonation string

	mu       sync.RWMutex
	settings map[string]string
}

// New loads the settings file (if present) and records the config defaults.
func New(path, defaultURL, defaultSession, defaultDonation string) *Settings {
	s := &Settings{
		path:            path,
		defaultURL:      defaultURL,
		defaultSession:  defaultSession,
		defaultDonation: defaultDonation,
		settings:        map[string]string{},
	}
	s.load()
	return s
}

func (s *Settings) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("dynset: failed to load, using config defaults", "err", err)
		} else {
			slog.Info("dynset: no settings file found, using config defaults")
		}
		return
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		slog.Warn("dynset: failed to parse, using config defaults", "err", err)
		return
	}
	s.settings = m
	slog.Info("dynset: loaded dynamic settings from file")
}

// save persists the current settings; the caller holds the write lock.
func (s *Settings) save() {
	b, err := json.MarshalIndent(s.settings, "", "  ")
	if err != nil {
		slog.Error("dynset: failed to marshal", "err", err)
		return
	}
	// The path can point into a mounted directory that exists only as a mount
	// point, so create the parent rather than losing the setting to ENOENT.
	if dir := filepath.Dir(s.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			slog.Error("dynset: failed to create settings dir", "dir", dir, "err", err)
			return
		}
	}
	if err := os.WriteFile(s.path, b, 0o600); err != nil {
		slog.Error("dynset: failed to save", "err", err)
		return
	}
	slog.Info("dynset: saved dynamic settings to file")
}

func (s *Settings) get(key, def string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.settings[key]; ok {
		return v
	}
	return def
}

func (s *Settings) set(key, val string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settings[key] = val
	s.save()
}

func (s *Settings) reset(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.settings[key]; ok {
		delete(s.settings, key)
		s.save()
	}
}

// AIURL returns the AI url, falling back to the config default.
func (s *Settings) AIURL() string { return s.get("ai_url", s.defaultURL) }

// SetAIURL sets and persists the AI url.
func (s *Settings) SetAIURL(url string) { s.set("ai_url", url) }

// ResetAIURL clears the AI url override.
func (s *Settings) ResetAIURL() { s.reset("ai_url") }

// AISessionID returns the AI session id, falling back to the config default.
func (s *Settings) AISessionID() string { return s.get("ai_session_id", s.defaultSession) }

// SetAISessionID sets and persists the AI session id.
func (s *Settings) SetAISessionID(id string) { s.set("ai_session_id", id) }

// ResetAISessionID clears the AI session id override.
func (s *Settings) ResetAISessionID() { s.reset("ai_session_id") }

// DonationEnabled reports whether the donation button should be shown under
// delivered messages. It defaults to FALSE: the button was removed by request,
// and an admin turns it back on with /admin_donate active. Living here rather
// than in the env config is deliberate — toggling it is a runtime decision that
// must not need a redeploy.
func (s *Settings) DonationEnabled() bool { return s.get("donation_enabled", "false") == "true" }

// SetDonationEnabled turns the donation button on or off and persists the choice.
func (s *Settings) SetDonationEnabled(on bool) {
	s.set("donation_enabled", strconv.FormatBool(on))
}

// MenuRichEnabled reports whether the /menu panel should be drawn as a Bot API
// 10.3 "rich message", with its buttons embedded in the text, instead of as a
// classic inline keyboard.
//
// It defaults to FALSE, and that default is a safety decision rather than a
// preference. A rich message carries no fallback content — no text and no
// reply_markup, only rich blocks — so a Telegram client too old to render one
// shows the user nothing they can tap. Since the panel is how users reach every
// feature, enabling this for everyone at once would strand whoever has not
// updated. Living here rather than in the env config means it can be turned on
// for a test audience and turned off again instantly, with no redeploy, if a
// single old-client report comes in.
func (s *Settings) MenuRichEnabled() bool { return s.get("menu_rich", "false") == "true" }

// SetMenuRichEnabled turns the embedded-button menu on or off and persists it.
func (s *Settings) SetMenuRichEnabled(on bool) {
	s.set("menu_rich", strconv.FormatBool(on))
}

// DonationLink returns the donation URL, falling back to the config default
// (DONATION_LINK) when an admin has not overridden it.
func (s *Settings) DonationLink() string { return s.get("donation_link", s.defaultDonation) }

// SetDonationLink overrides the donation URL.
func (s *Settings) SetDonationLink(url string) { s.set("donation_link", url) }

// ResetDonationLink drops the override, restoring the configured DONATION_LINK.
func (s *Settings) ResetDonationLink() { s.reset("donation_link") }
