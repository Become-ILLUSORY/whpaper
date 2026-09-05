package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

// detectResolutions asks the compositor for the physical mode of every connected
// output, so the exact-match `resolutions` filter returns pixel-perfect art.
// All helpers are best-effort: unknown desktop => empty list => config wins.
func detectResolutions(ctx context.Context) []string {
	var out []string
	if v, err := fromHyprctl(ctx); err == nil {
		out = append(out, v...)
	}
	if len(out) == 0 {
		if v, err := fromNiri(ctx); err == nil {
			out = append(out, v...)
		}
	}
	if len(out) == 0 {
		if v, err := fromWLRRandr(ctx); err == nil {
			out = append(out, v...)
		}
	}
	if len(out) == 0 {
		if v, err := fromXrandr(ctx); err == nil {
			out = append(out, v...)
		}
	}
	return dedupeByPixels(out)
}

func run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	if _, err := exec.LookPath(bin); err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, bin, args...).Output()
}

func fromHyprctl(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "hyprctl", "monitors", "-j")
	if err != nil {
		return nil, err
	}
	var monitors []struct {
		Name   string  `json:"name"`
		Width  int     `json:"width"`
		Height int     `json:"height"`
		Scale  float64 `json:"scale"`
	}
	if err := json.Unmarshal(out, &monitors); err != nil {
		return nil, err
	}
	var res []string
	for _, m := range monitors {
		if m.Width <= 0 || m.Height <= 0 {
			continue
		}
		s := m.Scale
		if s <= 0 {
			s = 1
		}
		res = append(res, fmt.Sprintf("%dx%d", int(float64(m.Width)*s+0.5), int(float64(m.Height)*s+0.5)))
	}
	return res, nil
}

func fromNiri(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "niri", "msg", "outputs", "--json")
	if err != nil {
		return nil, err
	}
	var outputs []struct {
		Name string `json:"name"`
		Mode struct {
			Width   int     `json:"width"`
			Height  int     `json:"height"`
			Refresh float64 `json:"refresh"`
		} `json:"mode"`
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		return nil, err
	}
	var res []string
	for _, o := range outputs {
		if o.Mode.Width > 0 && o.Mode.Height > 0 {
			res = append(res, fmt.Sprintf("%dx%d", o.Mode.Width, o.Mode.Height))
		}
	}
	return res, nil
}

func fromWLRRandr(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "wlr-randr", "--json")
	if err != nil {
		return nil, err
	}
	var outputs []struct {
		Name  string `json:"name"`
		Modes []struct {
			Width   int     `json:"width"`
			Height  int     `json:"height"`
			Refresh float64 `json:"refresh"`
			Current bool    `json:"current"`
		} `json:"modes"`
	}
	if err := json.Unmarshal(out, &outputs); err != nil {
		return nil, err
	}
	var res []string
	for _, o := range outputs {
		for _, m := range o.Modes {
			if m.Current && m.Width > 0 && m.Height > 0 {
				res = append(res, fmt.Sprintf("%dx%d", m.Width, m.Height))
			}
		}
	}
	return res, nil
}

func fromXrandr(ctx context.Context) ([]string, error) {
	out, err := run(ctx, "xrandr", "--current")
	if err != nil {
		return nil, err
	}
	var res []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, " connected ") {
			continue
		}
		// "DP-1 connected 3840x2160+0+0 ..."
		for _, tok := range strings.Fields(line) {
			if w, h, ok := parseWxH(tok); ok {
				res = append(res, fmt.Sprintf("%dx%d", w, h))
				break
			}
		}
	}
	return res, nil
}

func parseWxH(s string) (int, int, bool) {
	s = strings.TrimSpace(s)
	i := strings.IndexByte(s, 'x')
	if i <= 0 || i >= len(s)-1 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(s[:i])
	h, err2 := strconv.Atoi(s[i+1:])
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}

// dedupeByPixels removes duplicates and orders by descending pixel count so the
// primary (usually largest) monitor leads the filter list.
func dedupeByPixels(in []string) []string {
	seen := map[string]bool{}
	type entry struct {
		s      string
		pixels int
	}
	var list []entry
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		w, h, ok := parseWxH(v)
		if !ok {
			continue
		}
		list = append(list, entry{s: v, pixels: w * h})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].pixels > list[j].pixels })
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.s)
	}
	return out
}
