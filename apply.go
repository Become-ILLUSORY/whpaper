package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

func noctaliaBin(cfg Config) string {
	if b := strings.TrimSpace(cfg.NoctaliaBin); b != "" {
		return b
	}
	return "noctalia"
}

// noctaliaThemeMode asks the running shell for its resolved mode so the right
// directory_light / directory_dark bucket is used. Empty string when unavailable.
func noctaliaThemeMode(ctx context.Context, cfg Config) string {
	bin := noctaliaBin(cfg)
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, bin, "msg", "theme-mode-get").Output()
	if err != nil {
		return ""
	}
	s := strings.ToLower(string(out))
	switch {
	case strings.Contains(s, "light"):
		return "light"
	case strings.Contains(s, "dark"):
		return "dark"
	default:
		return ""
	}
}

// applyWallpaper talks to the shell over its own CLI, which is the documented
// stable surface (`noctalia msg wallpaper-set [connector] <path>`).
func applyWallpaper(ctx context.Context, cfg Config, path string, log *logger) error {
	bin := noctaliaBin(cfg)
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%s not found in PATH (install Noctalia or pass -no-apply)", bin)
	}
	args := []string{"msg", "wallpaper-set"}
	if cfg.Connector != "" {
		args = append(args, cfg.Connector)
	}
	args = append(args, path)
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w %s", bin, strings.Join(args, " "), err, truncate(string(out), 200))
	}
	log.Debugf("applied via %s", bin)
	return nil
}

func notify(ctx context.Context, cfg Config, log *logger, title, body, icon string) {
	if !cfg.Notify {
		return
	}
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		log.Debugf("notify-send missing, skipping notification")
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	args := []string{
		"--app-name=" + cfg.NotifyApp,
		"--replace-id=999",
		"--expire-time=4000",
		"--urgency=low",
	}
	if icon != "" {
		args = append(args, "--icon="+icon)
	}
	args = append(args, title, body)
	cmd := exec.CommandContext(cctx, bin, args...)
	if err := cmd.Run(); err != nil {
		log.Debugf("notify-send: %v", err)
	}
}

// runPostHook executes the user's shell command with the new wallpaper exported.
func runPostHook(ctx context.Context, hook, path string, log *logger) error {
	hook = strings.TrimSpace(hook)
	if hook == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/sh", "-c", hook)
	cmd.Env = append(os.Environ(), "WHPAPER_PATH="+path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("post_apply_hook: %w %s", err, truncate(string(out), 200))
	}
	if len(out) > 0 {
		log.Debugf("post_apply_hook: %s", truncate(string(out), 200))
	}
	return nil
}
