package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------- prefetch

func cmdPrefetch(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("prefetch", o, nil)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}
	stage := expandUser(cfg.StagingDir)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return fmt.Errorf("create staging %s: %w", stage, err)
	}
	dir := resolveDirectory(ctx, cfg)
	st := loadState()
	recent := st.recentSet()

	dl := newHTTPClient(cfg, time.Duration(cfg.TimeoutSeconds)*time.Second, log)
	client := NewClient(cfg, newHTTPClient(cfg, 15*time.Second, log), log)

	for attempt := 0; attempt < cfg.Retries; attempt++ {
		resp, base, err := client.Search(ctx, cfg.Search, st.LastEndpoint)
		if err != nil {
			log.Debugf("attempt %d: %v", attempt+1, err)
			continue
		}
		st.LastEndpoint = base
		cands := append([]Wallpaper{}, resp.Data...)
		rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
		for _, w := range cands {
			if recent[w.ID] {
				continue
			}
			if findExisting(dir, w) != "" {
				continue
			}
			final := stagePath(stage, w)
			if _, err := os.Stat(final); err == nil {
				continue
			}
			tmp := final + ".tmp"
			n, err := downloadFile(ctx, dl, rewriteImageURL(base, w.Path), tmp, cfg.MinFileBytes, cfg.MaxFileBytes, log, cfg.Token)
			if err != nil {
				log.Debugf("candidate %s: %v", w.ID, err)
				os.Remove(tmp)
				recent[w.ID] = true
				continue
			}
			if err := os.Rename(tmp, final); err != nil {
				os.Remove(tmp)
				return err
			}
			pruneStaging(stage, 1)
			if err := st.save(); err != nil {
				log.Warnf("state not saved: %v", err)
			}
			fmt.Printf("prefetched %s · %s · %s · %s\n", w.Resolution, w.ID, humanBytes(n), final)
			return nil
		}
	}
	return errors.New("nothing could be prefetched")
}

// ---------------------------------------------------------------- watch

func cmdWatch(ctx context.Context, args []string) error {
	o := newOpts()
	var jitter bool
	fs := newFlagSet("watch", o, func(f *flag.FlagSet) {
		f.DurationVar(&o.interval, "interval", 30*time.Minute, "rotation interval")
		f.BoolVar(&jitter, "jitter", true, "randomise each wait by ±10%")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}
	if o.interval <= 0 {
		return errors.New("-interval must be positive")
	}
	log.Infof("whpaper %s: rotating every %s (ctrl-c to stop)", version, o.interval)
	for {
		if _, err := runPipeline(ctx, cfg, o, log, 1); err != nil {
			log.Warnf("rotation failed: %v", err)
		}
		wait := o.interval
		if jitter {
			wait = time.Duration(float64(wait) * (0.9 + rand.Float64()*0.2))
		}
		select {
		case <-ctx.Done():
			log.Infof("stopped")
			return nil
		case <-time.After(wait):
		}
	}
}

// ---------------------------------------------------------------- config

func cmdConfig(ctx context.Context, args []string) error {
	o := newOpts()
	var doInit bool
	fs := newFlagSet("config", o, func(f *flag.FlagSet) {
		f.BoolVar(&doInit, "init", false, "write the default config file")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	path := o.cfgFile
	if path == "" {
		path = configPath()
	}
	if doInit {
		if _, err := os.Stat(path); err == nil && !o.force {
			return fmt.Errorf("%s already exists (pass -force to overwrite)", path)
		}
		cfg := defaultConfig()
		// directory stays empty on purpose: whpaper then follows Noctalia live.
		if err := cfg.save(path); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		fmt.Printf("detected wallpaper directory (Noctalia): %s\n", resolveDirectory(ctx, cfg))
		fmt.Println("set \"directory\" in the file only if you want to pin a different one.")
		return nil
	}
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}
	cfg.Directory = resolveDirectory(ctx, cfg)
	if cfg.APIKey != "" {
		cfg.APIKey = "<redacted>"
	}
	if cfg.Token != "" {
		cfg.Token = "<redacted>"
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

// ---------------------------------------------------------------- history

func cmdHistory(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("history", o, func(f *flag.FlagSet) {
		f.IntVar(&o.count, "n", 20, "entries to show")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	st := loadState()
	limit := o.count
	if limit <= 0 {
		limit = 20
	}
	if len(st.History) == 0 {
		fmt.Println("no history yet")
		return nil
	}
	fmt.Printf("%-19s %-8s %-9s %-10s %s\n", "TIME", "ID", "SIZE", "SOURCE", "PATH")
	for i, h := range st.History {
		if i >= limit {
			break
		}
		src := "applied"
		if _, err := os.Stat(h.Path); err != nil {
			src = "missing"
		}
		fmt.Printf("%-19s %-8s %-9s %-10s %s\n",
			h.AppliedAt.Format("2006-01-02 15:04:05"), h.ID, h.Resolution, src, h.Path)
	}
	return nil
}

// ---------------------------------------------------------------- doctor

type check struct {
	name   string
	ok     bool
	detail string
}

func (c check) line() string {
	mark := "✗"
	if c.ok {
		mark = "✓"
	}
	if c.detail == "" {
		return fmt.Sprintf("%s %s", mark, c.name)
	}
	return fmt.Sprintf("%s %-22s %s", mark, c.name, c.detail)
}

func cmdDoctor(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("doctor", o, nil)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}
	var checks []check
	var fatal bool

	path := o.cfgFile
	if path == "" {
		path = configPath()
	}
	if _, err := os.Stat(path); err == nil {
		checks = append(checks, check{"config", true, path})
	} else {
		checks = append(checks, check{"config", false, path + " (defaults in use; whpaper config -init)"})
	}

	dir := resolveDirectory(ctx, cfg)
	if st, err := os.Stat(dir); err == nil {
		ok := st.IsDir() && unixWritable(dir)
		checks = append(checks, check{"directory", ok, dir})
		if !ok {
			fatal = true
		}
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		checks = append(checks, check{"directory", false, dir + " (" + err.Error() + ")"})
		fatal = true
	} else {
		checks = append(checks, check{"directory", true, dir + " (created)"})
	}

	bin := noctaliaBin(cfg)
	if p, err := exec.LookPath(bin); err == nil {
		mode := noctaliaThemeMode(ctx, cfg)
		if mode == "" {
			checks = append(checks, check{"noctalia ipc", false, p + " (shell not running?)"})
		} else {
			checks = append(checks, check{"noctalia ipc", true, p + " · mode " + mode})
		}
	} else {
		checks = append(checks, check{"noctalia", false, "not in PATH — use -no-apply or set NOCTALIA_BIN"})
		if cfg.Apply {
			fatal = true
		}
	}

	if _, err := exec.LookPath("notify-send"); err == nil {
		checks = append(checks, check{"notify-send", true, ""})
	} else {
		checks = append(checks, check{"notify-send", false, "optional"})
	}

	if len(cfg.BestCFDomains) > 0 {
		union := unionPool(cfg.BestCFDomains)
		detail := make([]string, 0, len(cfg.BestCFDomains))
		for _, d := range cfg.BestCFDomains {
			detail = append(detail, fmt.Sprintf("%s(%d)", d, len(resolveIPPool(d))))
		}
		checks = append(checks, check{
			"best_cf_domain", len(union) > 0,
			fmt.Sprintf("%s → %d IPs", strings.Join(detail, " + "), len(union)),
		})
	}

	for _, base := range cfg.Endpoints {
		rep := testEndpoint(ctx, cfg, base, log)
		checks = append(checks, check{"api " + hostOf(base), rep.SearchOK, rep.String()})
	}

	for _, c := range checks {
		fmt.Println(c.line())
	}
	if fatal {
		return errors.New("some required checks failed")
	}
	return nil
}

func unixWritable(dir string) bool {
	probe := filepath.Join(dir, ".whpaper-write-test")
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(probe)
	return true
}
