package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const serviceTemplate = `[Unit]
Description=whpaper — random wallhaven wallpaper for Noctalia
After=graphical-session.target
Wants=graphical-session.target

[Service]
Type=oneshot
ExecStart=%s next
%s`

const timerTemplate = `[Unit]
Description=Rotate the wallhaven wallpaper (whpaper)

[Timer]
OnBootSec=%s
OnUnitActiveSec=%s
RandomizedDelaySec=%s
AccuracySec=1min
Unit=whpaper.service

[Install]
WantedBy=timers.target
`

func unitDir() string {
	return filepath.Join(configHome(), "systemd", "user")
}

func passthrough(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func cmdInstall(ctx context.Context, args []string) error {
	o := newOpts()
	var bin, extraEnv, onBoot, jitterFlag string
	fs := newFlagSet("install", o, func(f *flag.FlagSet) {
		f.StringVar(&bin, "bin", "", "whpaper binary to run (default: this executable)")
		f.DurationVar(&o.interval, "interval", 30*time.Minute, "rotation interval")
		f.StringVar(&extraEnv, "env", "", "extra Environment= entries, comma separated KEY=VALUE")
		f.StringVar(&onBoot, "on-boot", "2min", "delay before the first run after login/boot")
		f.StringVar(&jitterFlag, "random-delay", "2min", "max random extra delay per cycle")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}

	if bin == "" {
		exe, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot locate this binary, pass -bin: %w", err)
		}
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			bin = resolved
		} else {
			bin = exe
		}
	}
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("binary %s not found", bin)
	}

	envLines := ""
	for _, kv := range strings.Split(extraEnv, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		envLines += "Environment=" + kv + "\n"
	}
	if dir := resolveDirectory(ctx, cfg); dir != "" {
		if !strings.Contains(envLines, "WHPAPER_DIR") {
			envLines += "Environment=WHPAPER_DIR=" + dir + "\n"
		}
	}

	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("systemctl not found — run 'whpaper watch -interval ...' under your own supervisor instead")
	}
	dir := unitDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	svc := filepath.Join(dir, "whpaper.service")
	tmr := filepath.Join(dir, "whpaper.timer")
	if err := os.WriteFile(svc, []byte(fmt.Sprintf(serviceTemplate, fmt.Sprintf("%q", bin), envLines)), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(tmr, []byte(fmt.Sprintf(timerTemplate, onBoot, o.interval.String(), jitterFlag)), 0o644); err != nil {
		return err
	}
	log.Infof("wrote %s and %s", svc, tmr)

	if err := passthrough(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	if err := passthrough(ctx, "systemctl", "--user", "enable", "--now", "whpaper.timer"); err != nil {
		return err
	}
	_ = passthrough(ctx, "systemctl", "--user", "list-timers", "whpaper.timer", "--no-pager")
	fmt.Printf("\nNext steps:\n" +
		"  manual run : systemctl --user start whpaper.service\n" +
		"  logs       : journalctl --user -u whpaper.service -n 50\n" +
		"  hotkey     : bind an input to: whpaper next -silent\n")
	return nil
}

func cmdUninstall(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("uninstall", o, nil)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = passthrough(ctx, "systemctl", "--user", "disable", "--now", "whpaper.timer")
	}
	removed := 0
	for _, name := range []string{"whpaper.service", "whpaper.timer"} {
		p := filepath.Join(unitDir(), name)
		if err := os.Remove(p); err == nil {
			removed++
			log.Infof("removed %s", p)
		}
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = passthrough(ctx, "systemctl", "--user", "daemon-reload")
	}
	if removed == 0 {
		fmt.Println("nothing installed")
	}
	return nil
}
