package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	urlpkg "net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- state

type HistoryEntry struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	URL        string    `json:"url"`
	Resolution string    `json:"resolution"`
	Endpoint   string    `json:"endpoint,omitempty"`
	AppliedAt  time.Time `json:"applied_at"`
}

type State struct {
	RecentIDs    []string       `json:"recent_ids"`
	History      []HistoryEntry `json:"history"`
	LastEndpoint string         `json:"last_endpoint"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

func loadState() State {
	st := State{}
	data, err := os.ReadFile(statePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(data, &st) // a corrupt state file must never block a wallpaper change
	if st.RecentIDs == nil {
		st.RecentIDs = []string{}
	}
	return st
}

func (s State) save() error {
	s.UpdatedAt = time.Now()
	if err := os.MkdirAll(filepath.Dir(statePath()), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath())
}

func (s *State) remember(id string, window int) {
	out := make([]string, 0, len(s.RecentIDs)+1)
	out = append(out, id)
	for _, prev := range s.RecentIDs {
		if prev == id {
			continue
		}
		out = append(out, prev)
		if len(out) >= window {
			break
		}
	}
	s.RecentIDs = out
}

func (s *State) recentSet() map[string]bool {
	set := make(map[string]bool, len(s.RecentIDs))
	for _, id := range s.RecentIDs {
		set[id] = true
	}
	return set
}

// ---------------------------------------------------------------- files

// fileName is deterministic so a re-run can reuse an already downloaded image.
func fileName(w Wallpaper) string {
	return "wallhaven-" + w.ID + w.Ext()
}

func findExisting(dir string, w Wallpaper) string {
	p := filepath.Join(dir, fileName(w))
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		return p
	}
	return ""
}

func looksLikeImage(head []byte) bool {
	switch {
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return true // jpeg
	case len(head) >= 4 && head[0] == 0x89 && head[1] == 'P' && head[2] == 'N' && head[3] == 'G':
		return true // png
	case len(head) >= 3 && head[0] == 'G' && head[1] == 'I' && head[2] == 'F':
		return true // gif
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return true // webp
	case len(head) >= 12 && string(head[0:4]) == "\x00\x00\x00\x0C" && string(head[4:12]) == "JXL \r\n\x87\n":
		return true // jxl container
	case len(head) >= 2 && head[0] == 0xFF && head[1] == 0x0A:
		return true // jpeg-xl codestream
	case len(head) >= 2 && head[0] == 'B' && head[1] == 'M':
		return true // bmp
	default:
		return false
	}
}

func sniffFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, _ := io.ReadFull(f, buf)
	return looksLikeImage(buf[:n])
}

// downloadFile streams url into dst, refusing anything that is not a plausible
// image within the configured size window. token is the mirror access secret;
// it is only attached when the host is not wallhaven itself (so it never leaks
// to the upstream origin).
func downloadFile(ctx context.Context, client *http.Client, url, dst string, minBytes, maxBytes int64, log *logger, token string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "image/avif,image/webp,image/png,image/jpeg,*/*;q=0.8")
	if token != "" {
		if u, e := urlpkg.Parse(url); e == nil && !isWallhavenHost(u.Host) {
			req.Header.Set("x-wh-token", token)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, &apiError{Status: resp.StatusCode, Body: string(body)}
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(strings.ToLower(ct), "image/") {
		return 0, fmt.Errorf("unexpected content-type %q from %s", ct, url)
	}

	f, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	var src io.Reader = resp.Body
	if maxBytes > 0 {
		src = io.LimitReader(resp.Body, maxBytes+1)
	}
	start := time.Now()
	n, err := io.Copy(f, src)
	if err != nil {
		return n, err
	}
	if err := f.Sync(); err != nil {
		log.Debugf("sync %s: %v", dst, err)
	}
	if err := f.Close(); err != nil {
		return n, err
	}
	if maxBytes > 0 && n > maxBytes {
		os.Remove(dst)
		return n, fmt.Errorf("image larger than %d bytes", maxBytes)
	}
	if minBytes > 0 && n < minBytes {
		os.Remove(dst)
		return n, fmt.Errorf("image smaller than %d bytes", minBytes)
	}
	if !sniffFile(dst) {
		os.Remove(dst)
		return n, errors.New("payload is not a decodable image")
	}
	log.Debugf("fetched %s (%d bytes, %s)", url, n, time.Since(start).Round(100*time.Millisecond))
	return n, nil
}

// ---------------------------------------------------------------- retention

// prune keeps the newest cfg.Keep wallhaven images and removes the rest. Only
// files this tool created are ever touched.
func prune(dir string, keep int, log *logger) int {
	if keep <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	type item struct {
		path string
		mod  time.Time
	}
	var files []item
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "wallhaven-") {
			continue
		}
		if e.IsDir() {
			continue
		}
		if !isImageName(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, item{path: filepath.Join(dir, name), mod: info.ModTime()})
	}
	if len(files) <= keep {
		return 0
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	removed := 0
	for _, f := range files[keep:] {
		if err := os.Remove(f.path); err == nil {
			removed++
		} else {
			log.Warnf("prune %s: %v", f.path, err)
		}
	}
	return removed
}

func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".jpg", ".jpeg", ".png", ".webp", ".jxl", ".bmp", ".gif":
		return true
	default:
		return false
	}
}

// ---------------------------------------------------------------- staging

func stagePath(dir string, w Wallpaper) string {
	return filepath.Join(dir, fileName(w))
}

// takeStaged moves a prefetched file into the wallpaper directory.
func takeStaged(staged, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.Rename(staged, target); err == nil {
		return nil
	}
	// Cross-device cache: fall back to copy.
	in, err := os.Open(staged)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(target)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(target)
		return err
	}
	os.Remove(staged)
	return nil
}

// pruneStaging leaves at most keep files in the staging cache.
func pruneStaging(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "wallhaven-") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	sort.Slice(files, func(i, j int) bool {
		a, aerr := os.Stat(files[i])
		b, berr := os.Stat(files[j])
		if aerr != nil || berr != nil {
			return files[i] < files[j]
		}
		return a.ModTime().After(b.ModTime())
	})
	if keep < 0 {
		keep = 0
	}
	if len(files) <= keep {
		return
	}
	for _, f := range files[keep:] {
		os.Remove(f)
	}
}
