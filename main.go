// Command whpaper — random wallpaper from wallhaven.cc for the Noctalia shell.
//
// Zero external dependencies: standard library only, so the binary cross-compiles
// anywhere with CGO_ENABLED=0.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var userAgent = "whpaper/" + version + " (+https://github.com/Become-ILLUSORY/whpaper)"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := realMain(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "whpaper: "+err.Error())
		os.Exit(1)
	}
}

func realMain(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			io.WriteString(os.Stdout, usage)
			return nil
		case "-v", "--version", "version":
			fmt.Printf("whpaper %s (commit %s, built %s)\n", version, commit, date)
			return nil
		}
	}

	cmd, rest := "next", args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, rest = args[0], args[1:]
	}

	switch cmd {
	case "next", "run":
		return cmdNext(ctx, rest)
	case "prefetch":
		return cmdPrefetch(ctx, rest)
	case "watch":
		return cmdWatch(ctx, rest)
	case "probe":
		return cmdProbe(ctx, rest)
	case "config":
		return cmdConfig(ctx, rest)
	case "history":
		return cmdHistory(ctx, rest)
	case "doctor":
		return cmdDoctor(ctx, rest)
	case "install":
		return cmdInstall(ctx, rest)
	case "uninstall":
		return cmdUninstall(ctx, rest)
	default:
		io.WriteString(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// opts holds every command-line override. Empty/zero values mean "not set", so the
// config file keeps authority.
type opts struct {
	cfgFile     string
	endpoint    string
	directory   string
	connector   string
	bestCF      string
	resolutions string
	ratios      string
	categories  string
	purity      string
	atleast     string
	colors      string
	query       string
	sorting     string
	topRange    string
	keep        int
	retries     int
	timeout     int
	count       int
	interval    time.Duration
	notify      bool
	silent      bool
	noApply     bool
	dryRun      bool
	autoRes     bool
	jsonOut     bool
	verbose     bool
	save        bool
	force       bool
}

func newOpts() *opts {
	return &opts{
		notify: true, keep: 0, retries: 0, timeout: 0, count: 1,
		interval: 30 * time.Minute,
	}
}

func newFlagSet(name string, o *opts, extra func(*flag.FlagSet)) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&o.cfgFile, "config", "", "path to config file (default $XDG_CONFIG_HOME/whpaper/config.json)")
	fs.StringVar(&o.endpoint, "endpoint", "", "force a single API base URL (bypasses failover)")
	fs.StringVar(&o.directory, "dir", "", "directory to store wallpapers in")
	fs.StringVar(&o.connector, "connector", "", "noctalia output name, e.g. DP-1 (empty = all monitors)")
	fs.StringVar(&o.bestCF, "best-cf", "", "comma-separated hostnames whose A records are your preferred Cloudflare IPs")
	fs.StringVar(&o.resolutions, "resolutions", "", "exact resolution list, e.g. 3840x2160,2560x1440")
	fs.StringVar(&o.ratios, "ratios", "", "aspect ratio list, e.g. 16x9")
	fs.StringVar(&o.categories, "categories", "", "3-digit mask general/anime/people (1 = include)")
	fs.StringVar(&o.purity, "purity", "", "3-digit mask sfw/sketchy/nsfw (1 = include)")
	fs.StringVar(&o.atleast, "atleast", "", "minimum resolution, e.g. 2560x1440 (mutually exclusive with -resolutions)")
	fs.StringVar(&o.colors, "colors", "", "hex colour filter, e.g. 42413c or random")
	fs.StringVar(&o.query, "q", "", "search terms")
	fs.StringVar(&o.sorting, "sorting", "", "random | relevance | date_added | views | favorites | toplist")
	fs.StringVar(&o.topRange, "top-range", "", "toplist time window: 1d 3d 1w 1M 3M 6M 1y (default 1M)")
	fs.IntVar(&o.keep, "keep", 0, "how many downloaded wallpapers to retain (0 = config value)")
	fs.IntVar(&o.retries, "retries", 0, "candidate attempts per run (0 = config value)")
	fs.IntVar(&o.timeout, "timeout", 0, "per-request timeout in seconds (0 = config value)")
	fs.BoolVar(&o.notify, "notify", true, "send desktop notifications")
	fs.BoolVar(&o.silent, "silent", false, "shorthand for -notify=false")
	fs.BoolVar(&o.noApply, "no-apply", false, "download only, do not touch the wallpaper")
	fs.BoolVar(&o.dryRun, "dry-run", false, "print the chosen wallpaper without downloading")
	fs.BoolVar(&o.autoRes, "auto-resolution", false, "derive -resolutions from the connected monitors")
	fs.BoolVar(&o.jsonOut, "json", false, "machine-readable JSON output")
	fs.BoolVar(&o.verbose, "v", false, "verbose logging")
	fs.BoolVar(&o.force, "force", false, "ignore the recent-id dedup list")
	if extra != nil {
		extra(fs)
	}
	return fs
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return err
	}
	return nil
}

const usage = `whpaper — random wallhaven.cc wallpapers for Noctalia

Usage:
  whpaper [command] [flags]

Commands:
  next        fetch one random wallpaper, download it and apply it (default)
  prefetch    download the next candidate into the staging cache, do not apply
  watch       stay in foreground and rotate every -interval
  probe       measure every endpoint / fronting IP and report the fastest
  config      show the effective configuration (-init writes the default file)
  history     list recently applied wallpapers
  doctor      check binary, directories, noctalia IPC and network reachability
  install     install a systemd user service + timer
  uninstall   remove them
  help        this text

Everyday flags:
  -dir string        wallpaper directory (default: auto from Noctalia)
  -keep int          downloaded files to retain
  -q string          search terms
  -resolutions list  exact sizes, e.g. 3840x2160,2560x1440
  -atleast WxH       minimum size (alternative to -resolutions)
  -categories 010    general/anime/people mask
  -purity 100        sfw/sketchy/nsfw mask
  -silent            no desktop notification
  -v                 verbose logging

Advanced flags:
  -connector DP-1    apply to one output only    -no-apply   download only
  -dry-run           resolve without downloading  -json      machine-readable output
  -auto-resolution   derive sizes from monitors   -force     ignore recent dedup
  -endpoint URL      pin one API base             -config p  alternate config file
  -best-cf a,b       front requests through hostnames whose A records are your
                     preferred Cloudflare IPs (see "best_cf_domain" in config)

Examples:
  whpaper next
  whpaper next -resolutions 3840x2160 -categories 010
  whpaper probe -v -save
  whpaper install -interval 15m
  whpaper next -json
`
