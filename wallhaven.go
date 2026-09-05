package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Thumbs / Wallpaper mirror the /api/v1/search payload. Field set verified against
// the live API on 2026-09-05.
type Thumbs struct {
	Large    string `json:"large"`
	Original string `json:"original"`
	Small    string `json:"small"`
}

type Wallpaper struct {
	ID         string   `json:"id"`
	URL        string   `json:"url"`
	ShortURL   string   `json:"short_url"`
	Views      int      `json:"views"`
	Favorites  int      `json:"favorites"`
	Source     string   `json:"source"`
	Purity     string   `json:"purity"`
	Category   string   `json:"category"`
	DimensionX int      `json:"dimension_x"`
	DimensionY int      `json:"dimension_y"`
	Resolution string   `json:"resolution"`
	Ratio      string   `json:"ratio"`
	FileSize   int64    `json:"file_size"`
	FileType   string   `json:"file_type"`
	CreatedAt  string   `json:"created_at"`
	Colors     []string `json:"colors"`
	Path       string   `json:"path"`
	Thumbs     Thumbs   `json:"thumbs"`
}

// Ext returns the on-disk extension implied by the CDN path.
func (w Wallpaper) Ext() string {
	if u, err := url.Parse(w.Path); err == nil {
		if dot := strings.LastIndexByte(u.Path, '.'); dot >= 0 && dot < len(u.Path)-1 {
			ext := strings.ToLower(u.Path[dot:])
			if len(ext) <= 5 {
				return ext
			}
		}
	}
	switch strings.ToLower(w.FileType) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

type metaInfo struct {
	CurrentPage int    `json:"current_page"`
	LastPage    int    `json:"last_page"`
	PerPage     int    `json:"per_page"`
	Total       int    `json:"total"`
	Seed        string `json:"seed"`
}

type searchResponse struct {
	Data []Wallpaper `json:"data"`
	Meta metaInfo    `json:"meta"`
}

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("http %d: %s", e.Status, truncate(e.Body, 160))
}

type Client struct {
	http   *http.Client
	bases  []string
	apiKey string
	token  string
	log    *logger
}

func NewClient(cfg Config, client *http.Client, log *logger) *Client {
	bases := make([]string, 0, len(cfg.Endpoints))
	for _, b := range cfg.Endpoints {
		b = strings.TrimRight(strings.TrimSpace(b), "/")
		if b != "" {
			bases = append(bases, b)
		}
	}
	if len(bases) == 0 {
		bases = defaultConfig().Endpoints
	}
	return &Client{http: client, bases: bases, apiKey: cfg.APIKey, token: cfg.Token, log: log}
}

// buildQuery drops empty values; wallhaven ignores unknown keys, so we never send them.
func buildQuery(s SearchConfig) url.Values {
	q := url.Values{}
	set := func(k, v string) {
		v = strings.TrimSpace(v)
		if v != "" {
			q.Set(k, v)
		}
	}
	set("sorting", s.Sorting)
	set("order", s.Order)
	set("purity", s.Purity)
	set("categories", s.Categories)
	set("ratios", s.Ratios)
	set("resolutions", s.Resolutions)
	set("atleast", s.AtLeast)
	set("colors", s.Colors)
	set("q", s.Query)
	set("topRange", s.TopRange)
	if s.Page > 1 {
		q.Set("page", strconv.Itoa(s.Page))
	}
	return q
}

// newRequest targets one endpoint + path. The mirror token is only ever sent to
// the mirror, never to wallhaven.
func (c *Client) newRequest(ctx context.Context, base, path string, q url.Values) (*http.Request, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("bad endpoint %q: %w", base, err)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.token != "" && !isWallhavenHost(u.Host) {
		req.Header.Set("x-wh-token", c.token)
	}
	return req, nil
}

func (c *Client) searchOnce(ctx context.Context, base string, q url.Values) (*searchResponse, error) {
	req, err := c.newRequest(ctx, base, "/api/v1/search", q)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &apiError{Status: resp.StatusCode, Body: string(body)}
	}
	var out searchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", base, err)
	}
	if len(out.Data) == 0 {
		return &out, errors.New("empty result set (filters too narrow?)")
	}
	return &out, nil
}

// Search walks the endpoint list starting at preferred (index, may be negative)
// and returns the first successful page together with the base that served it.
func (c *Client) Search(ctx context.Context, s SearchConfig, preferred string) (*searchResponse, string, error) {
	q := buildQuery(s)
	order := c.bases
	if i := indexOfBase(c.bases, preferred); i > 0 {
		order = append(append([]string{}, c.bases[i:]...), c.bases[:i]...)
	}
	var errs []string
	for _, base := range order {
		start := time.Now()
		res, err := c.searchOnce(ctx, base, q)
		if err == nil {
			c.log.Debugf("search via %s in %s (%d hits, seed %s)", base, time.Since(start).Round(time.Millisecond), res.Meta.Total, res.Meta.Seed)
			return res, base, nil
		}
		c.log.Debugf("search via %s failed: %v", base, err)
		errs = append(errs, fmt.Sprintf("%s: %v", base, err))
		if ctx.Err() != nil {
			break
		}
	}
	if len(errs) == 0 {
		return nil, "", errors.New("no endpoints configured")
	}
	return nil, "", errors.New(strings.Join(errs, " | "))
}

func indexOfBase(bases []string, want string) int {
	want = strings.TrimRight(strings.TrimSpace(want), "/")
	if want == "" {
		return -1
	}
	for i, b := range bases {
		if b == want {
			return i
		}
	}
	return -1
}
