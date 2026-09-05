package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SearchConfig maps onto the wallhaven /api/v1/search query parameters that are
// worth exposing to users. Empty fields are omitted from the request.
type SearchConfig struct {
	Sorting     string `json:"sorting"`
	Purity      string `json:"purity"`
	Categories  string `json:"categories"`
	Ratios      string `json:"ratios"`
	Resolutions string `json:"resolutions"`
	AtLeast     string `json:"atleast"`
	Colors      string `json:"colors"`
	Query       string `json:"q"`

	// Rarely-needed knobs: settable via flags/env, never written to the file.
	Order    string `json:"-"`
	TopRange string `json:"-"`
	Page     int    `json:"-"`
}

// Config is the user-facing file. Only the fields below appear in
// ~/.config/whpaper/config.json; the rest of the runtime knobs live in
// defaultConfig() and can be overridden by flags or WHPAPER_* env vars, so the
// config file stays short and readable.
type Config struct {
	// Endpoints are tried in order. Put a self-hosted mirror first if
	// wallhaven.cc is blocked on your network (see proxy/ for a Cloudflare one).
	Endpoints []string `json:"endpoints"`
	// APIKey is your wallhaven account API key — only needed for NSFW results
	// or a higher request quota. Optional.
	APIKey string `json:"api_key,omitempty"`
	// Token is the access secret for a mirror endpoint (x-wh-token). Optional.
	Token  string       `json:"token,omitempty"`
	Search SearchConfig `json:"search"`

	Directory string `json:"directory,omitempty"` // empty = auto-detect from Noctalia
	Keep      int    `json:"keep,omitempty"`      // downloaded files to retain
	Notify    bool   `json:"notify"`              // desktop notification on change

	// BestCFDomain is a hostname whose A/AAAA records are Cloudflare IPs that
	// work from your network (a "优选 IP" pool). When set, every wallhaven/mirror
	// request is dialed through those IPs with the real SNI — no VPN needed.
	BestCFDomain string `json:"best_cf_domain,omitempty"`

	// ---- runtime-only (flags / env / defaults), not part of the config file ----
	Apply          bool   `json:"-"`
	Connector      string `json:"-"`
	NoctaliaBin    string `json:"-"`
	NotifyApp      string `json:"-"`
	Retries        int    `json:"-"`
	TimeoutSeconds int    `json:"-"`
	MinFileBytes   int64  `json:"-"`
	MaxFileBytes   int64  `json:"-"`
	Proxy          string `json:"-"`
	PostApplyHook  string `json:"-"`
	AutoResolution bool   `json:"-"`
	StagingDir     string `json:"-"`
	UseStaging     bool   `json:"-"`
	RecentWindow   int    `json:"-"`
}

func defaultConfig() Config {
	return Config{
		Endpoints: []string{"https://wallhaven.cc"},
		Search: SearchConfig{
			Sorting:     "random",
			Purity:      "100",
			Categories:  "110",
			Ratios:      "16x9",
			Resolutions: "1920x1080,2560x1440,3840x2160",
		},
		Keep:           40,
		Notify:         true,
		Apply:          true,
		NoctaliaBin:    "noctalia",
		NotifyApp:      "whpaper",
		Retries:        6,
		TimeoutSeconds: 45,
		MinFileBytes:   32 * 1024,
		MaxFileBytes:   48 * 1024 * 1024,
		StagingDir:     filepath.Join(cacheHome(), "whpaper", "staging"),
		UseStaging:     true,
		RecentWindow:   400,
	}
}

// ---------------------------------------------------------------- paths

func xdgDir(envVar, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		if filepath.IsAbs(v) {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = "."
	}
	return filepath.Join(home, fallback)
}

func configHome() string { return xdgDir("XDG_CONFIG_HOME", ".config") }
func stateHome() string  { return xdgDir("XDG_STATE_HOME", ".local/state") }
func cacheHome() string  { return xdgDir("XDG_CACHE_HOME", ".cache") }

func configPath() string {
	return filepath.Join(configHome(), "whpaper", "config.json")
}

func statePath() string {
	return filepath.Join(stateHome(), "whpaper", "state.json")
}

func expandUser(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return p
	}
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// ---------------------------------------------------------------- load / save

func loadConfigFile(path string) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	// Strict=false keeps forward-compatible upgrades from breaking older builds.
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	applyDefaults(&cfg)
	return cfg, nil
}

// applyDefaults repairs fields a partial config file may have zeroed out.
func applyDefaults(cfg *Config) {
	def := defaultConfig()
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = def.Endpoints
	}
	if cfg.Keep <= 0 {
		cfg.Keep = def.Keep
	}
	if cfg.Retries <= 0 {
		cfg.Retries = def.Retries
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = def.TimeoutSeconds
	}
	if cfg.MinFileBytes <= 0 {
		cfg.MinFileBytes = def.MinFileBytes
	}
	if cfg.MaxFileBytes <= 0 {
		cfg.MaxFileBytes = def.MaxFileBytes
	}
	if cfg.RecentWindow <= 0 {
		cfg.RecentWindow = def.RecentWindow
	}
	if strings.TrimSpace(cfg.NoctaliaBin) == "" {
		cfg.NoctaliaBin = def.NoctaliaBin
	}
	if strings.TrimSpace(cfg.NotifyApp) == "" {
		cfg.NotifyApp = def.NotifyApp
	}
	if strings.TrimSpace(cfg.StagingDir) == "" {
		cfg.StagingDir = def.StagingDir
	}
	if strings.TrimSpace(cfg.Search.Sorting) == "" {
		cfg.Search.Sorting = def.Search.Sorting
	}
	for i, e := range cfg.Endpoints {
		cfg.Endpoints[i] = strings.TrimRight(strings.TrimSpace(e), "/")
	}
}

func (c Config) save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// applyEnv layers WHPAPER_* environment variables on top of the file config.
func (c *Config) applyEnv() {
	if v := strings.TrimSpace(os.Getenv("WHPAPER_ENDPOINT")); v != "" {
		parts := strings.Split(v, ",")
		list := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimRight(strings.TrimSpace(p), "/")
			if p != "" {
				list = append(list, p)
			}
		}
		if len(list) > 0 {
			c.Endpoints = list
		}
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_API_KEY")); v != "" {
		c.APIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_TOKEN")); v != "" {
		c.Token = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_DIR")); v != "" {
		c.Directory = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_PROXY")); v != "" {
		c.Proxy = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_BEST_CF_DOMAIN")); v != "" {
		c.BestCFDomain = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_KEEP")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Keep = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_NO_APPLY")); v == "1" || strings.EqualFold(v, "true") {
		c.Apply = false
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_CONNECTOR")); v != "" {
		c.Connector = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_TIMEOUT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.TimeoutSeconds = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_RETRIES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retries = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_POST_APPLY_HOOK")); v != "" {
		c.PostApplyHook = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_AUTO_RESOLUTION")); v == "1" || strings.EqualFold(v, "true") {
		c.AutoResolution = true
	}
	if v := strings.TrimSpace(os.Getenv("NOCTALIA_BIN")); v != "" {
		c.NoctaliaBin = v
	}
	if v := strings.TrimSpace(os.Getenv("WHPAPER_NOTIFY")); v == "0" || strings.EqualFold(v, "false") {
		c.Notify = false
	}
}

// buildConfig resolves config file + env + flags into the final configuration.
func buildConfig(o *opts) (Config, error) {
	path := o.cfgFile
	if path == "" {
		path = configPath()
	}
	cfg, err := loadConfigFile(path)
	if err != nil {
		return cfg, err
	}
	cfg.applyEnv()

	if o.endpoint != "" {
		cfg.Endpoints = []string{strings.TrimRight(strings.TrimSpace(o.endpoint), "/")}
	}
	if o.directory != "" {
		cfg.Directory = o.directory
	}
	if o.connector != "" {
		cfg.Connector = o.connector
	}
	if o.bestCF != "" {
		cfg.BestCFDomain = strings.TrimSpace(o.bestCF)
	}
	if o.keep > 0 {
		cfg.Keep = o.keep
	}
	if o.retries > 0 {
		cfg.Retries = o.retries
	}
	if o.timeout > 0 {
		cfg.TimeoutSeconds = o.timeout
	}
	if o.silent {
		cfg.Notify = false
	}
	if !o.notify {
		cfg.Notify = false
	}
	if o.noApply {
		cfg.Apply = false
	}
	if o.autoRes {
		cfg.AutoResolution = true
	}

	s := &cfg.Search
	if o.resolutions != "" {
		s.Resolutions = o.resolutions
		s.AtLeast = ""
	}
	if o.ratios != "" {
		s.Ratios = o.ratios
	}
	if o.categories != "" {
		s.Categories = o.categories
	}
	if o.purity != "" {
		s.Purity = o.purity
	}
	if o.atleast != "" {
		s.AtLeast = o.atleast
		s.Resolutions = ""
	}
	if o.colors != "" {
		s.Colors = o.colors
	}
	if o.query != "" {
		s.Query = o.query
	}
	if o.sorting != "" {
		s.Sorting = o.sorting
	}

	applyDefaults(&cfg)
	return cfg, nil
}

// ---------------------------------------------------------------- directory resolution

// resolveDirectory mirrors Noctalia's own precedence: an explicit config value,
// then the shell's [wallpaper] directory (theme-mode aware), then XDG Pictures.
func resolveDirectory(ctx context.Context, cfg Config) string {
	if d := expandPath(cfg.Directory); d != "" {
		return d
	}
	dirs := noctaliaWallpaperDirs(ctx, cfg)
	if len(dirs) > 0 {
		mode := noctaliaThemeMode(ctx, cfg)
		if v := dirs[dirKeyForMode(mode)]; v != "" {
			return v
		}
		if v := dirs["directory"]; v != "" {
			return v
		}
	}
	if v := xdgPicturesDir(); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "Wallpapers"
	}
	return filepath.Join(home, "Pictures", "Wallpapers")
}

func dirKeyForMode(mode string) string {
	switch mode {
	case "light":
		return "directory_light"
	case "dark":
		return "directory_dark"
	default:
		return "directory"
	}
}

// noctaliaWallpaperDirs returns the [wallpaper] directory keys exactly as the
// shell resolves them. Noctalia persists GUI/IPC changes into settings.toml in
// the STATE dir (~/.local/state/noctalia) — that override layer must win; the
// config dir only holds hand-written drop-ins. Reading only the config dir is
// the classic way to end up watching the wrong folder.
func noctaliaWallpaperDirs(ctx context.Context, cfg Config) map[string]string {
	out := map[string]string{}
	// Preferred: ask the shell for its fully merged config (handles includes and
	// precedence exactly). Fall back to reading the layers ourselves.
	if merged := noctaliaMergedConfig(ctx, cfg); merged != "" {
		parseWallpaperDirs(merged, out)
		if out["directory"] != "" || out["directory_light"] != "" || out["directory_dark"] != "" {
			return out
		}
	}
	for _, path := range noctaliaConfigFiles() {
		if data, err := os.ReadFile(path); err == nil {
			parseWallpaperDirs(string(data), out)
		}
	}
	return out
}

// noctaliaConfigFiles lists the TOML layers in load order: every *.toml in the
// config dir (alphabetical), then the state-dir settings.toml overrides last.
func noctaliaConfigFiles() []string {
	var files []string
	dir := noctaliaConfigDir()
	if entries, err := os.ReadDir(dir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".toml") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, filepath.Join(dir, n))
		}
	}
	return append(files, filepath.Join(noctaliaStateDir(), "settings.toml"))
}

func noctaliaConfigDir() string {
	if v := strings.TrimSpace(os.Getenv("NOCTALIA_CONFIG_HOME")); v != "" {
		return filepath.Join(v, "noctalia")
	}
	return filepath.Join(configHome(), "noctalia")
}

func noctaliaStateDir() string {
	if v := strings.TrimSpace(os.Getenv("NOCTALIA_STATE_HOME")); v != "" {
		return filepath.Join(v, "noctalia")
	}
	return filepath.Join(stateHome(), "noctalia")
}

// noctaliaMergedConfig runs `noctalia config export` and returns its TOML.
func noctaliaMergedConfig(ctx context.Context, cfg Config) string {
	bin := noctaliaBin(cfg)
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "config", "export").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// parseWallpaperDirs is a tiny TOML reader scoped to the [wallpaper] section's
// directory keys. Values are expanded (~, $HOME, $XDG_*, $VAR) like Noctalia does.
func parseWallpaperDirs(text string, out map[string]string) {
	section := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.Trim(line, "[]"))
			continue
		}
		if section != "wallpaper" {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key != "directory" && key != "directory_light" && key != "directory_dark" {
			continue
		}
		val := strings.TrimSpace(line[eq+1:])
		if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		if v := expandPath(val); v != "" {
			out[key] = v
		}
	}
}

// expandPath resolves ~, $HOME, the XDG base-directory tokens and generic
// $VAR / ${VAR} references, mirroring Noctalia's FileUtils expansion.
func expandPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	if s == "" {
		return s
	}
	if s == "~" || strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if s == "~" {
				s = home
			} else {
				s = filepath.Join(home, s[2:])
			}
		}
	}
	return expandDollarVars(s)
}

func expandDollarVars(s string) string {
	if !strings.ContainsRune(s, '$') {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '$' {
			b.WriteByte(s[i])
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' {
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				b.WriteByte('$')
				i++
				continue
			}
			b.WriteString(resolveVar(s[i+2 : i+end]))
			i += end + 1
			continue
		}
		j := i + 1
		for j < len(s) && isVarChar(s[j]) {
			j++
		}
		if j == i+1 {
			b.WriteByte('$')
			i++
			continue
		}
		b.WriteString(resolveVar(s[i+1 : j]))
		i = j
	}
	return b.String()
}

func isVarChar(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func resolveVar(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	switch name {
	case "HOME":
		return home
	case "XDG_CONFIG_HOME":
		return filepath.Join(home, ".config")
	case "XDG_DATA_HOME":
		return filepath.Join(home, ".local", "share")
	case "XDG_STATE_HOME":
		return filepath.Join(home, ".local", "state")
	case "XDG_CACHE_HOME":
		return filepath.Join(home, ".cache")
	default:
		return ""
	}
}

func xdgPicturesDir() string {
	if v := strings.TrimSpace(os.Getenv("XDG_PICTURES_DIR")); v != "" {
		return expandPath(v)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(configHome(), "user-dirs.dirs"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "XDG_PICTURES_DIR=") {
			continue
		}
		return expandPath(strings.TrimPrefix(line, "XDG_PICTURES_DIR="))
	}
	return ""
}
