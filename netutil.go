package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// ipDialer keeps the real hostname for SNI/Host/verification while dialing an
// explicit IP pool. That is the "优选 CF IP" trick: point best_cf_domain at a
// hostname whose A records are Cloudflare IPs that work from your network, and
// every wallhaven/mirror request is fronted through them without trusting DNS.
type ipDialer struct {
	base   *net.Dialer
	pool   []string
	fronts func(host string) bool
	log    *logger
}

func (d *ipDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return d.base.DialContext(ctx, network, addr)
	}
	if len(d.pool) == 0 || d.fronts == nil || !d.fronts(strings.ToLower(host)) {
		return d.base.DialContext(ctx, network, addr)
	}
	var lastErr error
	// d.pool is ordered fastest-first (see frontingPool); try in that order.
	for _, candidate := range d.pool {
		ip := strings.TrimSpace(candidate)
		if ip == "" || net.ParseIP(ip) == nil {
			continue
		}
		// Bound each attempt so a dead or stalled IP in the pool is skipped in a
		// few seconds instead of eating the whole request timeout.
		actx, cancel := context.WithTimeout(ctx, 4*time.Second)
		conn, e := d.base.DialContext(actx, network, net.JoinHostPort(ip, port))
		cancel()
		if e == nil {
			d.log.Debugf("front %s → %s (best_cf pool %d)", host, ip, len(d.pool))
			return conn, nil
		}
		d.log.Debugf("front %s via %s failed: %v", host, ip, e)
		lastErr = e
	}
	if lastErr != nil {
		return nil, fmt.Errorf("dial %s via best-CF pool: %w", host, lastErr)
	}
	return nil, fmt.Errorf("no valid IP in best-CF pool for %s", host)
}

// resolveIPPool looks up the A/AAAA records of the best_cf_domain. Results are
// cached briefly so a run that builds several clients does not re-query DNS.
var (
	poolMu    sync.Mutex
	poolCache = map[string]poolEntry{}
)

type poolEntry struct {
	ips []string
	at  time.Time
}

func resolveIPPool(domain string) []string {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return nil
	}
	poolMu.Lock()
	if e, ok := poolCache[domain]; ok && time.Since(e.at) < 5*time.Minute {
		poolMu.Unlock()
		return e.ips
	}
	poolMu.Unlock()

	seen := map[string]bool{}
	var out []string
	add := func(ips []string) {
		for _, ip := range ips {
			ip = strings.TrimSpace(ip)
			if ip == "" || seen[ip] || net.ParseIP(ip) == nil {
				continue
			}
			seen[ip] = true
			out = append(out, ip)
		}
	}
	// System resolver first (fast, no TLS), then DoH to recover records the local
	// resolver truncates or withholds — a hostname with dozens of A records often
	// comes back as 2-3 from glibc/systemd-resolved but in full over DoH.
	add(systemHosts(domain))
	add(dohHosts(domain))

	poolMu.Lock()
	poolCache[domain] = poolEntry{ips: out, at: time.Now()}
	poolMu.Unlock()
	return out
}

func systemHosts(domain string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		return nil
	}
	return ips
}

// dohClient is deliberately plain (no fronting — that would be circular) but it
// honours HTTPS_PROXY so it works behind a tunnel too.
var dohClient = &http.Client{
	Timeout:   3 * time.Second,
	Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
}

func dohServers() []string {
	if v := strings.TrimSpace(os.Getenv("WHPAPER_DOH")); v != "" {
		var list []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				list = append(list, s)
			}
		}
		return list
	}
	// AliDNS first: reachable from mainland China without a VPN. Cloudflare and
	// Google cover the rest of the world.
	return []string{
		"https://dns.alidns.com/resolve",
		"https://cloudflare-dns.com/dns-query",
		"https://dns.google/resolve",
	}
}

// dohHosts queries the DoH resolvers in parallel and unions their A/AAAA answers.
func dohHosts(domain string) []string {
	type answer struct {
		Type int    `json:"type"`
		Data string `json:"data"`
	}
	type payload struct {
		Answer []answer `json:"Answer"`
	}

	var (
		mu  sync.Mutex
		out []string
		wg  sync.WaitGroup
	)
	for _, ep := range dohServers() {
		wg.Add(1)
		go func(ep string) {
			defer wg.Done()
			u := ep + "?name=" + url.QueryEscape(domain) + "&type=A"
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return
			}
			req.Header.Set("accept", "application/dns-json")
			resp, err := dohClient.Do(req)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return
			}
			var p payload
			if json.Unmarshal(body, &p) != nil {
				return
			}
			var ips []string
			for _, a := range p.Answer {
				if a.Type == 1 || a.Type == 28 {
					ips = append(ips, a.Data)
				}
			}
			mu.Lock()
			out = append(out, ips...)
			mu.Unlock()
		}(ep)
	}
	wg.Wait()
	return out
}

// frontingPool returns the best_cf IP pool ordered fastest-first, so a run stops
// landing on a slow or half-dead IP at random. Latencies are measured once and
// cached for a few minutes.
var (
	frontMu    sync.Mutex
	frontCache = map[string]poolEntry{}
)

func frontingPool(cfg Config, log *logger) []string {
	if cfg.BestCFDomain == "" {
		return nil
	}
	raw := resolveIPPool(cfg.BestCFDomain)
	if len(raw) <= 1 {
		return raw
	}
	key := cfg.BestCFDomain
	frontMu.Lock()
	if e, ok := frontCache[key]; ok && time.Since(e.at) < 10*time.Minute {
		frontMu.Unlock()
		return e.ips
	}
	frontMu.Unlock()

	ordered := orderIPsByLatency(raw, log)
	frontMu.Lock()
	frontCache[key] = poolEntry{ips: ordered, at: time.Now()}
	frontMu.Unlock()
	return ordered
}

// orderIPsByLatency dials every IP in parallel (TLS handshake to a wallhaven
// SNI, which is the same CF anycast the mirror uses) and returns them fastest
// first, unreachable ones last so they remain as a fallback.
func orderIPsByLatency(ips []string, log *logger) []string {
	type res struct {
		ip string
		d  time.Duration
		ok bool
	}
	ch := make(chan res, len(ips))
	var wg sync.WaitGroup
	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			defer cancel()
			d, err := dialTLSLatency(ctx, ip, "wallhaven.cc", 2500*time.Millisecond)
			ch <- res{ip: ip, d: d, ok: err == nil}
		}(ip)
	}
	wg.Wait()
	close(ch)

	var good, bad []res
	for r := range ch {
		if r.ok {
			good = append(good, r)
		} else {
			bad = append(bad, r)
		}
	}
	sort.SliceStable(good, func(i, j int) bool { return good[i].d < good[j].d })
	out := make([]string, 0, len(ips))
	for _, r := range good {
		out = append(out, r.ip)
	}
	for _, r := range bad {
		out = append(out, r.ip)
	}
	if len(good) > 0 {
		log.Debugf("best_cf pool ranked: %s (%dms) … %d reachable / %d total",
			good[0].ip, good[0].d.Milliseconds(), len(good), len(ips))
	}
	return out
}

// endpointHosts returns the bare hostnames of the configured endpoints so the
// dialer knows which non-wallhaven hosts (a self-hosted mirror) to front.
func endpointHosts(endpoints []string) map[string]bool {
	set := map[string]bool{}
	for _, e := range endpoints {
		if u, err := url.Parse(e); err == nil {
			if h := strings.ToLower(u.Hostname()); h != "" {
				set[h] = true
			}
		}
	}
	return set
}

func newHTTPClient(cfg Config, timeout time.Duration, log *logger) *http.Client {
	tr := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          8,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   12 * time.Second,
		ExpectContinueTimeout: time.Second,
		DisableCompression:    false,
	}
	dialer := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}
	if cfg.BestCFDomain != "" {
		pool := frontingPool(cfg, log)
		if len(pool) > 0 {
			log.Debugf("best_cf fronting active: %s → %d IPs", cfg.BestCFDomain, len(pool))
			fronts := endpointHosts(cfg.Endpoints)
			tr.DialContext = (&ipDialer{
				base: dialer,
				pool: pool,
				fronts: func(host string) bool {
					return isWallhavenHost(host) || fronts[host]
				},
				log: log,
			}).DialContext
		} else {
			log.Warnf("best_cf_domain %s resolved to no IPs — dialing normally", cfg.BestCFDomain)
			tr.DialContext = dialer.DialContext
		}
	} else {
		tr.DialContext = dialer.DialContext
	}
	switch {
	case cfg.Proxy != "":
		if u, err := url.Parse(cfg.Proxy); err == nil && u.Scheme != "" {
			tr.Proxy = http.ProxyURL(u)
		} else {
			tr.Proxy = http.ProxyFromEnvironment
		}
	default:
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func isWallhavenHost(h string) bool {
	h = strings.ToLower(h)
	for _, root := range []string{"wallhaven.cc", "whvn.cc"} {
		if h == root || strings.HasSuffix(h, "."+root) {
			return true
		}
	}
	return false
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// rewriteImageURL points a wallhaven asset URL at the mirror that served the API
// response. Mirrors that already rewrite their JSON are a no-op here.
func rewriteImageURL(apiBase, asset string) string {
	au, err := url.Parse(asset)
	if err != nil {
		return asset
	}
	bu, err := url.Parse(apiBase)
	if err != nil || bu.Host == "" {
		return asset
	}
	if au.Host == bu.Host || !isWallhavenHost(au.Host) {
		return asset
	}
	out := *au
	out.Host = bu.Host
	out.Scheme = bu.Scheme
	return out.String()
}

// dialTLSLatency measures TCP connect + TLS handshake to ip, advertising hostname
// as SNI. Used by `probe` to rank fronting IPs.
func dialTLSLatency(ctx context.Context, ip, hostname string, timeout time.Duration) (time.Duration, error) {
	if net.ParseIP(ip) == nil {
		return 0, fmt.Errorf("%q is not an IP literal", ip)
	}
	start := time.Now()
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, "443"))
	if err != nil {
		return 0, err
	}
	tc := tls.Client(conn, &tls.Config{
		ServerName: hostname,
		NextProtos: []string{"h2", "http/1.1"},
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return 0, err
	}
	elapsed := time.Since(start)
	_ = tc.Close()
	return elapsed, nil
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
