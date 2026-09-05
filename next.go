package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Result struct {
	ID         string   `json:"id"`
	Page       string   `json:"page"`
	ImageURL   string   `json:"image_url"`
	LocalPath  string   `json:"local_path"`
	Resolution string   `json:"resolution"`
	Category   string   `json:"category"`
	Purity     string   `json:"purity"`
	Bytes      int64    `json:"bytes"`
	Source     string   `json:"source"`
	Endpoint   string   `json:"endpoint"`
	Applied    bool     `json:"applied"`
	Removed    int      `json:"pruned"`
	Colors     []string `json:"colors,omitempty"`
	ElapsedMS  int64    `json:"elapsed_ms"`
}

func cmdNext(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("next", o, func(f *flag.FlagSet) {
		f.IntVar(&o.count, "count", 1, "fetch several wallpapers at once (apply only affects the last)")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}
	n := o.count
	if n < 1 {
		n = 1
	}
	results, err := runPipeline(ctx, cfg, o, log, n)
	for _, r := range results {
		printResult(r, o.jsonOut)
	}
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return errors.New("no wallpaper fetched")
	}
	return nil
}

// runPipeline is shared by next / prefetch / watch.
func runPipeline(ctx context.Context, cfg Config, o *opts, log *logger, count int) ([]Result, error) {
	// Instant acknowledgement: ranking the优选 pool and the search itself take a
	// second or two, so don't leave the user staring at nothing. replace-id makes
	// this morph into "开始下载" and then the result instead of stacking up.
	notify(ctx, cfg, log, "正在获取壁纸", "连接 wallhaven 并挑选中…", "preferences-desktop-wallpaper")

	dir := resolveDirectory(ctx, cfg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	st := loadState()

	if cfg.AutoResolution && strings.TrimSpace(o.resolutions) == "" {
		if res := detectResolutions(ctx); len(res) > 0 {
			cfg.Search.Resolutions = strings.Join(res, ",")
			cfg.Search.AtLeast = ""
			log.Debugf("auto resolution -> %s", cfg.Search.Resolutions)
		} else {
			log.Debugf("auto resolution: no compositor found, keeping configured list")
		}
	}

	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	dl := newHTTPClient(cfg, timeout, log)
	api := newHTTPClient(cfg, 15*time.Second, log)
	client := NewClient(cfg, api, log)

	var results []Result
	var lastErr error
	for i := 0; i < count; i++ {
		res, err := fetchOnce(ctx, cfg, o, log, dir, &st, client, dl)
		if err != nil {
			lastErr = err
			break
		}
		if res == nil {
			continue
		}
		results = append(results, *res)
	}
	return results, lastErr
}

func fetchOnce(ctx context.Context, cfg Config, o *opts, log *logger, dir string, st *State, client *Client, dl *http.Client) (*Result, error) {
	started := time.Now()
	recent := st.recentSet()
	var lastApplyErr error

	for attempt := 0; attempt < cfg.Retries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s := cfg.Search
		s.Page = 1
		resp, base, err := client.Search(ctx, s, st.LastEndpoint)
		if err != nil {
			lastErr := err
			log.Debugf("attempt %d: %v", attempt+1, lastErr)
			if wait, ok := rateLimitWait(lastErr); ok {
				log.Warnf("rate limited by %s, waiting %s", base, wait)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(wait):
				}
			}
			continue
		}
		st.LastEndpoint = base

		cands := make([]Wallpaper, len(resp.Data))
		copy(cands, resp.Data)
		rand.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })

		for _, w := range cands {
			if !o.force && recent[w.ID] {
				log.Debugf("skip %s (shown recently)", w.ID)
				continue
			}
			if o.dryRun {
				r := Result{
					ID: w.ID, Page: w.URL, ImageURL: rewriteImageURL(base, w.Path),
					Resolution: w.Resolution, Category: w.Category, Purity: w.Purity,
					Bytes: w.FileSize, Source: "dry-run", Endpoint: base, Colors: w.Colors,
					ElapsedMS: time.Since(started).Milliseconds(),
				}
				return &r, nil
			}

			// Give feedback before a possibly multi-second network fetch (a cached
			// file resolves instantly, so skip the notice then).
			if findExisting(dir, w) == "" {
				if _, e := os.Stat(stagePath(expandUser(cfg.StagingDir), w)); e != nil {
					notify(ctx, cfg, log, "开始下载", wallpaperSummary(w), "folder-download")
				}
			}

			path, source, bytes, err := acquire(ctx, cfg, log, dir, base, w, dl)
			if err != nil {
				log.Debugf("candidate %s failed: %v", w.ID, err)
				recent[w.ID] = true
				continue
			}

			res := Result{
				ID: w.ID, Page: w.URL, ImageURL: rewriteImageURL(base, w.Path), LocalPath: path,
				Resolution: w.Resolution, Category: w.Category, Purity: w.Purity,
				Bytes: bytes, Source: source, Endpoint: base, Colors: w.Colors,
			}

			if cfg.Apply {
				if err := applyWallpaper(ctx, cfg, path, log); err != nil {
					log.Warnf("%v", err)
					// The file is on disk; keep it and let the next candidate try.
					recent[w.ID] = true
					lastApplyErr = err
					continue
				}
				res.Applied = true
			}
			if err := runPostHook(ctx, cfg.PostApplyHook, path, log); err != nil {
				log.Warnf("%v", err)
			}

			res.Removed = prune(dir, cfg.Keep, log)
			st.remember(w.ID, cfg.RecentWindow)
			st.History = append([]HistoryEntry{{
				ID: w.ID, Path: path, URL: w.URL, Resolution: w.Resolution,
				Endpoint: base, AppliedAt: time.Now(),
			}}, trimHistory(st.History, 50)...)
			if err := st.save(); err != nil {
				log.Warnf("state not saved: %v", err)
			}
			res.ElapsedMS = time.Since(started).Milliseconds()

			title := "壁纸已切换"
			if !res.Applied {
				title = "壁纸已下载"
			}
			notify(ctx, cfg, log, title, wallpaperSummary(w), "user-desktop")
			return &res, nil
		}
		log.Debugf("attempt %d exhausted all %d candidates", attempt+1, len(cands))
	}

	err := errors.New("no wallpaper could be fetched")
	if lastApplyErr != nil {
		err = fmt.Errorf("no wallpaper could be fetched (last apply error: %v)", lastApplyErr)
	}
	ftitle, fbody := failureNotice(err)
	notify(ctx, cfg, log, ftitle, fbody, "dialog-warning")
	return nil, err
}

// wallpaperSummary is the human-facing notification body: category + resolution,
// no opaque wallhaven id.
func wallpaperSummary(w Wallpaper) string {
	cat := categoryCN(w.Category)
	res := prettyResolution(w.Resolution)
	if cat == "" {
		return res
	}
	return cat + " · " + res
}

func categoryCN(c string) string {
	switch strings.ToLower(c) {
	case "anime":
		return "动漫"
	case "general":
		return "通用"
	case "people":
		return "人物"
	default:
		return ""
	}
}

func prettyResolution(r string) string {
	return strings.ReplaceAll(r, "x", " × ")
}

// failureNotice maps a raw error to a short (title, body) for the failure popup.
func failureNotice(err error) (string, string) {
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "403") || strings.Contains(low, "token"):
		return "下载失败", "反代口令无效，请检查 token 配置"
	case strings.Contains(low, "no such host") || strings.Contains(low, "dial") ||
		strings.Contains(low, "timeout") || strings.Contains(low, "connection") ||
		strings.Contains(low, "reset"):
		return "下载失败", "网络不可达，可尝试镜像或优选 IP（whpaper probe 体检）"
	case strings.Contains(low, "not found in path") || strings.Contains(low, "noctalia"):
		return "切换失败", "未找到 Noctalia，或桌面未运行"
	case strings.Contains(low, "empty result") || strings.Contains(low, "filters"):
		return "下载失败", "筛选条件过窄，没有匹配的壁纸"
	default:
		return "下载失败", "获取失败，运行 whpaper next -v 查看详情"
	}
}

// acquire returns the local path for a candidate, preferring an already present
// file, then the prefetch cache, then the network.
func acquire(ctx context.Context, cfg Config, log *logger, dir, base string, w Wallpaper, dl *http.Client) (string, string, int64, error) {
	if p := findExisting(dir, w); p != "" {
		info, err := os.Stat(p)
		if err == nil && info.Size() >= cfg.MinFileBytes {
			log.Debugf("reusing %s", p)
			return p, "existing", info.Size(), nil
		}
		os.Remove(p)
	}

	if cfg.UseStaging {
		sp := stagePath(expandUser(cfg.StagingDir), w)
		if info, err := os.Stat(sp); err == nil && info.Size() >= cfg.MinFileBytes {
			target := filepath.Join(dir, fileName(w))
			if err := takeStaged(sp, target); err == nil {
				log.Debugf("staged hit %s", sp)
				return target, "staging", info.Size(), nil
			} else {
				log.Debugf("staging move failed: %v", err)
			}
		}
	}

	target := filepath.Join(dir, fileName(w))
	tmp := filepath.Join(dir, ".whpaper-"+w.ID+"-"+fmt.Sprint(time.Now().UnixNano())+".tmp")
	url := rewriteImageURL(base, w.Path)
	n, err := downloadFile(ctx, dl, url, tmp, cfg.MinFileBytes, cfg.MaxFileBytes, log, cfg.Token)
	if err != nil {
		os.Remove(tmp)
		return "", "", 0, err
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", "", n, fmt.Errorf("rename %s: %w", target, err)
	}
	return target, "download", n, nil
}

func trimHistory(h []HistoryEntry, max int) []HistoryEntry {
	if len(h) <= max {
		return h
	}
	return h[:max]
}

func rateLimitWait(err error) (time.Duration, bool) {
	var ae *apiError
	if !errors.As(err, &ae) {
		return 0, false
	}
	if ae.Status == http.StatusTooManyRequests || ae.Status == 429 {
		return 20 * time.Second, true
	}
	return 0, false
}

func printResult(r Result, asJSON bool) {
	if asJSON {
		data, err := json.MarshalIndent(r, "", "  ")
		if err == nil {
			fmt.Println(string(data))
		}
		return
	}
	state := "saved"
	switch {
	case r.Applied:
		state = "applied"
	case r.Source == "dry-run":
		state = "dry-run"
	}
	line := fmt.Sprintf("%s %s · %s · %s · %s", state, r.Resolution, r.ID, humanBytes(r.Bytes), r.LocalPath)
	if r.Source == "staging" {
		line += " (prefetched)"
	}
	if r.Removed > 0 {
		line += fmt.Sprintf(" [-%d old]", r.Removed)
	}
	fmt.Println(line)
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
