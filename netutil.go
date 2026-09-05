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
	"sync/atomic"
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
	cursor uint32 // rotates the starting IP so retries don't wedge on one bad host
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
	// d.pool is ordered fastest-first (see frontingPool). Start each new dial at
	// a rotating offset so a single IP that TCP-connects but resets the request
	// cannot wedge every retry — the next attempt naturally tries the next IP.
	n := len(d.pool)
	start := int(atomic.AddUint32(&d.cursor, 1)-1) % n
	for k := 0; k < n; k++ {
		ip := strings.TrimSpace(d.pool[(start+k)%n])
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
	if len(cfg.BestCFDomains) == 0 {
		return nil
	}
	raw := unionPool(cfg.BestCFDomains)
	if len(raw) <= 1 {
		return raw
	}
	// Probe the host we will actually front (the primary endpoint), so an IP that
	// only serves some other name is not falsely ranked good.
	probeHost := "wallhaven.cc"
	if len(cfg.Endpoints) > 0 {
		if h := hostOf(cfg.Endpoints[0]); h != "" {
			probeHost = h
		}
	}
	key := strings.Join(cfg.BestCFDomains, ",") + "|" + probeHost
	frontMu.Lock()
	if e, ok := frontCache[key]; ok && time.Since(e.at) < 10*time.Minute {
		frontMu.Unlock()
		return e.ips
	}
	frontMu.Unlock()

	ordered := orderIPsByLatency(raw, probeHost, log)
	frontMu.Lock()
	frontCache[key] = poolEntry{ips: ordered, at: time.Now()}
	frontMu.Unlock()
	return ordered
}

// unionPool resolves every best_cf domain and merges the IPs (deduped, order
// preserved) so several优选 pools can be combined.
func unionPool(domains []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range domains {
		for _, ip := range resolveIPPool(d) {
			if !seen[ip] {
				seen[ip] = true
				out = append(out, ip)
			}
		}
	}
	return out
}

// orderIPsByLatency probes every IP in parallel with a real HTTPS request to
// wallhaven.cc and returns them fastest-first, unreachable ones last. A plain
// TLS handshake is not enough: some IPs (e.g. a proxy service that is not a
// Cloudflare pool) finish the handshake but reset the HTTP request, so we must
// exercise the full request path to rank honestly.
func orderIPsByLatency(ips []string, probeHost string, log *logger) []string {
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
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			d, err := httpProbeLatency(ctx, ip, probeHost, 3*time.Second)
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
	if len(cfg.BestCFDomains) > 0 {
		pool := frontingPool(cfg, log)
		domains := strings.Join(cfg.BestCFDomains, ", ")
		if len(pool) > 0 {
			log.Debugf("best_cf fronting active: %s → %d IPs", domains, len(pool))
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
			log.Warnf("best_cf_domain %s resolved to no IPs — dialing normally", domains)
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

// httpProbeLatency performs a real HTTPS GET to https://<hostname>/ through a
// specific IP (keeping the true SNI and certificate verification) and returns the
// round-trip time. Unlike a bare TLS handshake it catches IPs that complete the
// handshake but reset the HTTP request — the failure mode of a non-Cloudflare
// "proxy" pool masquerading as a优选 IP list.
func httpProbeLatency(ctx context.Context, ip, hostname string, timeout time.Duration) (time.Duration, error) {
	if net.ParseIP(ip) == nil {
		return 0, fmt.Errorf("%q is not an IP literal", ip)
	}
	start := time.Now()
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
		TLSClientConfig:   &tls.Config{ServerName: hostname},
		DialContext: func(c context.Context, _, _ string) (net.Conn, error) {
			d := &net.Dialer{Timeout: timeout}
			return d.DialContext(c, "tcp", net.JoinHostPort(ip, "443"))
		},
	}
	cl := &http.Client{Transport: tr, Timeout: timeout}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+hostname+"/", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0") // 1-byte body; not the rate-limited /api path
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	if resp.StatusCode >= 500 {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	return time.Since(start), nil
}

func isTimeout(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
