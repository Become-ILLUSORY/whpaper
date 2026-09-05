package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

type endpointReport struct {
	Base     string  `json:"base"`
	SearchOK bool    `json:"search_ok"`
	SearchMS int64   `json:"search_ms"`
	Total    int     `json:"total"`
	ImageOK  bool    `json:"image_ok"`
	ImageMS  int64   `json:"image_ms"`
	KBps     float64 `json:"kbps"`
	Err      string  `json:"error,omitempty"`
}

func (r endpointReport) String() string {
	if !r.SearchOK {
		return fmt.Sprintf("search failed (%s)", truncate(r.Err, 70))
	}
	s := fmt.Sprintf("search %dms, %d hits", r.SearchMS, r.Total)
	if r.ImageOK {
		s += fmt.Sprintf(" · image %dms %.0fKB/s", r.ImageMS, r.KBps)
	} else {
		s += " · image failed (" + truncate(r.Err, 60) + ")"
	}
	return s
}

// testEndpoint exercises both halves of the mirror: the JSON API and the CDN
// stream, because a worker can proxy one and still fail the other.
func testEndpoint(ctx context.Context, cfg Config, base string, log *logger) endpointReport {
	rep := endpointReport{Base: base}
	s := SearchConfig{
		Sorting:     "random",
		Purity:      "100",
		Categories:  "010",
		Resolutions: "1920x1080",
	}
	client := NewClient(cfg, newHTTPClient(cfg, 20*time.Second, log), log)
	start := time.Now()
	resp, used, err := client.Search(ctx, s, "")
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	rep.SearchOK = true
	rep.SearchMS = time.Since(start).Milliseconds()
	rep.Total = resp.Meta.Total
	if len(resp.Data) == 0 {
		rep.Err = "no candidates"
		return rep
	}

	w := resp.Data[0]
	imgURL := rewriteImageURL(used, w.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Range", "bytes=0-262143")
	if cfg.Token != "" && !isWallhavenHost(hostOf(imgURL)) {
		req.Header.Set("x-wh-token", cfg.Token)
	}
	dl := newHTTPClient(cfg, 25*time.Second, log)
	t0 := time.Now()
	r2, err := dl.Do(req)
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK && r2.StatusCode != http.StatusPartialContent {
		rep.Err = fmt.Sprintf("image http %d", r2.StatusCode)
		return rep
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r2.Body, 256*1024))
	if err != nil {
		rep.Err = err.Error()
		return rep
	}
	elapsed := time.Since(t0)
	rep.ImageOK = true
	rep.ImageMS = elapsed.Milliseconds()
	if elapsed.Seconds() > 0 {
		rep.KBps = float64(n) / 1024 / elapsed.Seconds()
	}
	return rep
}

func cmdProbe(ctx context.Context, args []string) error {
	o := newOpts()
	fs := newFlagSet("probe", o, func(f *flag.FlagSet) {
		f.BoolVar(&o.save, "save", false, "write the fastest endpoint back to the config file")
	})
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	log := newLogger(o.verbose)
	cfg, err := buildConfig(o)
	if err != nil {
		return err
	}

	var reps []endpointReport
	for _, base := range cfg.Endpoints {
		log.Debugf("probing %s", base)
		rep := testEndpoint(ctx, cfg, base, log)
		fmt.Printf("%-34s %s\n", base, rep.String())
		reps = append(reps, rep)
	}

	if cfg.BestCFDomain != "" {
		pool := resolveIPPool(cfg.BestCFDomain)
		sni := ""
		if len(cfg.Endpoints) > 0 {
			sni = hostOf(cfg.Endpoints[0])
		}
		fmt.Printf("\nbest_cf_domain %s → %d IPs (fronting SNI %s)\n", cfg.BestCFDomain, len(pool), sni)
		if len(pool) == 0 {
			fmt.Println("  (no A/AAAA records resolved)")
		} else {
			type ipResult struct {
				ip  string
				d   time.Duration
				err error
			}
			results := make([]ipResult, len(pool))
			var wg sync.WaitGroup
			sem := make(chan struct{}, 16)
			for i, ip := range pool {
				wg.Add(1)
				go func(i int, ip string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
					defer cancel()
					d, err := dialTLSLatency(cctx, ip, sni, 5*time.Second)
					results[i] = ipResult{ip: ip, d: d, err: err}
				}(i, ip)
			}
			wg.Wait()
			sort.SliceStable(results, func(i, j int) bool {
				bi, bj := results[i].err != nil, results[j].err != nil
				if bi != bj {
					return !bi
				}
				return results[i].d < results[j].d
			})
			reachable, dead := 0, 0
			for _, r := range results {
				if r.err != nil {
					dead++
					fmt.Printf("  %-22s ✗ %v\n", r.ip, r.err)
					continue
				}
				reachable++
				fmt.Printf("  %-22s ✓ %dms\n", r.ip, r.d.Milliseconds())
			}
			summary := fmt.Sprintf("pool health: %d/%d reachable", reachable, len(results))
			if reachable > 0 {
				summary += fmt.Sprintf(", fastest %dms, slowest ok %dms",
					results[0].d.Milliseconds(), results[reachable-1].d.Milliseconds())
			}
			if dead > 0 {
				summary += fmt.Sprintf(", %d dead", dead)
			}
			fmt.Println("\n" + summary)
		}
	}

	if o.jsonOut {
		data, err := json.MarshalIndent(reps, "", "  ")
		if err == nil {
			fmt.Println(string(data))
		}
	}

	best, bestScore := -1, 0.0
	for i, r := range reps {
		if !r.SearchOK || !r.ImageOK {
			continue
		}
		score := float64(r.SearchMS) + float64(r.ImageMS)
		if best < 0 || score < bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return errors.New("no endpoint usable from this network (try a mirror, WHPAPER_PROXY, or best_cf_domain fronting)")
	}
	fmt.Printf("\nbest: %s (%.0fms round trip)\n", reps[best].Base, bestScore)

	if o.save {
		path := o.cfgFile
		if path == "" {
			path = configPath()
		}
		fileCfg, err := loadConfigFile(path)
		if err != nil {
			return err
		}
		rest := make([]string, 0, len(fileCfg.Endpoints))
		for _, b := range fileCfg.Endpoints {
			if b != reps[best].Base {
				rest = append(rest, b)
			}
		}
		fileCfg.Endpoints = append([]string{reps[best].Base}, rest...)
		applyDefaults(&fileCfg)
		if err := fileCfg.save(path); err != nil {
			return err
		}
		fmt.Printf("primary endpoint saved to %s\n", path)
	}
	return nil
}
