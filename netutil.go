package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
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
	for _, idx := range rand.Perm(len(d.pool)) {
		ip := strings.TrimSpace(d.pool[idx])
		if ip == "" || net.ParseIP(ip) == nil {
			continue
		}
		conn, e := d.base.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if e == nil {
			return conn, nil
		}
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

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	looked, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil {
		return nil
	}
	ips := make([]string, 0, len(looked))
	for _, ip := range looked {
		if net.ParseIP(ip) != nil {
			ips = append(ips, ip)
		}
	}
	poolMu.Lock()
	poolCache[domain] = poolEntry{ips: ips, at: time.Now()}
	poolMu.Unlock()
	return ips
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

func newHTTPClient(cfg Config, timeout time.Duration) *http.Client {
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
	if pool := resolveIPPool(cfg.BestCFDomain); len(pool) > 0 {
		fronts := endpointHosts(cfg.Endpoints)
		tr.DialContext = (&ipDialer{
			base: dialer,
			pool: pool,
			fronts: func(host string) bool {
				return isWallhavenHost(host) || fronts[host]
			},
		}).DialContext
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
