// Package webfetch downloads a web page for the model (the web_fetch
// tool) and turns it into readable text.
//
// The server fetches whatever URL a model asks for, so the fetch must not
// become a way into the server's own network (SSRF): only http(s) on the
// standard ports, and every connection -- redirects included -- is
// checked against private, loopback and other non-public addresses at
// dial time, after DNS resolution, so a hostname that resolves (or
// re-resolves) to an internal address is refused too. No cookies, no
// credentials, no proxy. Pages are read as served: no JavaScript.
package webfetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

// Page is a fetched page as text.
type Page struct {
	URL       string // as requested (normalized)
	FinalURL  string // after redirects
	Title     string
	Text      string
	Truncated bool // Text was cut at Fetcher.MaxChars
	FetchedAt time.Time
}

// Defaults for a zero Fetcher field.
const (
	DefaultMaxBytes  = 5 << 20 // raw body read from the network
	DefaultMaxChars  = 100_000 // extracted text kept
	DefaultTimeout   = 20 * time.Second
	DefaultUserAgent = "Mozilla/5.0 (compatible; NeoChat/1.0; page reader for an AI assistant)"
	maxRedirects     = 5
)

// Fetcher fetches pages. The zero value is ready to use.
type Fetcher struct {
	MaxBytes  int64
	MaxChars  int
	Timeout   time.Duration
	UserAgent string

	// AllowPrivate turns the address and port checks off. Only for
	// tests, which serve pages from loopback on random ports.
	AllowPrivate bool

	once   sync.Once
	client *http.Client
}

// Error is a fetch failure worth showing to the model and the user as
// is ("HTTP 404", "PDF documents can't be read").
type Error struct{ Message string }

func (e *Error) Error() string { return e.Message }

func failf(format string, args ...any) error { return &Error{Message: fmt.Sprintf(format, args...)} }

var errBlockedAddress = errors.New("webfetch: address is not public")

// NormalizeURL checks that raw is a fetchable http(s) URL and returns it
// without its fragment.
func NormalizeURL(raw string) (string, error) {
	return normalizeURL(raw, false)
}

// Normalize is NormalizeURL with this fetcher's checks (relaxed for
// AllowPrivate test fetchers).
func (f *Fetcher) Normalize(raw string) (string, error) {
	return normalizeURL(raw, f.AllowPrivate)
}

func normalizeURL(raw string, relaxed bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", failf("not a valid web address: %q", raw)
	}
	if err := checkURL(u, relaxed); err != nil {
		return "", describeError(err, context.Background())
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
}

// checkURL vets a URL before it is requested (and on every redirect);
// relaxed is the test mode that skips the port and address checks.
func checkURL(u *url.URL, relaxed bool) error {
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return failf("only http and https pages can be read, not %s:", u.Scheme)
	}
	if u.User != nil {
		return failf("addresses with a username or password can't be read")
	}
	if u.Hostname() == "" {
		return failf("the address has no host")
	}
	if relaxed {
		return nil
	}
	switch u.Port() {
	case "", "80", "443":
	default:
		return failf("only the standard web ports (80, 443) can be read, not %s", u.Port())
	}
	// An IP written into the URL is refused up front; hostnames are
	// checked once resolved, at dial time.
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && Blocked(ip) {
		return errBlockedAddress
	}
	return nil
}

// checkDialAddress is the per-connection check: address is the resolved
// "ip:port" about to be dialed.
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return errBlockedAddress
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || Blocked(ip) {
		return errBlockedAddress
	}
	return nil
}

// Blocked reports whether ip is not a public unicast address: loopback,
// private, link-local, multicast, carrier-grade NAT, documentation and
// reserved ranges, including IPv4 addresses tunnelled in IPv6.
func Blocked(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(ip) {
			return true
		}
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case nat64.Contains(ip):
			return Blocked(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case sixToFour.Contains(ip):
			return Blocked(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}

var (
	blockedPrefixes = mustPrefixes(
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"100::/64", "2001::/32", "2001:db8::/32", "fec0::/10",
	)
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

func mustPrefixes(list ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(list))
	for i, p := range list {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

func (f *Fetcher) httpClient() *http.Client {
	f.once.Do(f.buildClient)
	return f.client
}

func (f *Fetcher) buildClient() {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if !f.AllowPrivate {
		// Control runs for every connection attempt with the resolved
		// address, so the check can't be dodged by DNS tricks.
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			return checkDialAddress(address)
		}
	}
	transport := &http.Transport{
		Proxy:                 nil, // a proxy would connect for us, past the address check
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
	f.client = &http.Client{
		Transport: transport,
		Jar:       nil,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return failf("too many redirects")
			}
			return checkURL(req.URL, f.AllowPrivate)
		},
	}
}

// Fetch downloads rawURL and extracts its text.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (Page, error) {
	normalized, err := normalizeURL(rawURL, f.AllowPrivate)
	if err != nil {
		return Page{}, err
	}
	timeout := f.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, normalized, nil)
	if err != nil {
		return Page{}, failf("not a valid web address: %q", rawURL)
	}
	agent := f.UserAgent
	if agent == "" {
		agent = DefaultUserAgent
	}
	req.Header.Set("User-Agent", agent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.8,*/*;q=0.5")
	req.Header.Set("Accept-Language", "en,ru;q=0.8,*;q=0.5")

	resp, err := f.httpClient().Do(req)
	if err != nil {
		return Page{}, describeError(err, ctx)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Page{}, failf("the site answered HTTP %d", resp.StatusCode)
	}

	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Page{}, describeError(err, ctx)
	}
	cutBody := int64(len(body)) > maxBytes
	if cutBody {
		// A cut mid-character would make UTF-8 text look like a legacy
		// encoding to the charset sniffer.
		body = trimPartialRune(body[:maxBytes])
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType, _, _ = mime.ParseMediaType(http.DetectContentType(body))
	}
	page := Page{URL: normalized, FinalURL: resp.Request.URL.String(), FetchedAt: time.Now()}
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		decoded, err := charset.NewReader(bytes.NewReader(body), contentType)
		if err != nil {
			return Page{}, failf("the page's text encoding isn't supported")
		}
		page.Title, page.Text = extractHTML(decoded, resp.Request.URL)
	case isTextType(mediaType):
		decoded, err := charset.NewReader(bytes.NewReader(body), contentType)
		if err != nil {
			return Page{}, failf("the page's text encoding isn't supported")
		}
		raw, _ := io.ReadAll(decoded)
		page.Text = normalizeText(strings.ToValidUTF8(string(raw), ""))
	case mediaType == "application/pdf":
		return Page{}, failf("PDF documents can't be read, only web pages and text files")
	default:
		return Page{}, failf("this kind of file (%s) can't be read, only web pages and text files", mediaType)
	}

	maxChars := f.MaxChars
	if maxChars <= 0 {
		maxChars = DefaultMaxChars
	}
	if utf8.RuneCountInString(page.Text) > maxChars {
		page.Text = string([]rune(page.Text)[:maxChars])
		page.Truncated = true
	}
	page.Truncated = page.Truncated || cutBody
	if strings.TrimSpace(page.Text) == "" {
		return Page{}, failf("the page has no readable text (it may need JavaScript)")
	}
	return page, nil
}

func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i]
			}
			break
		}
	}
	return b
}

func isTextType(mediaType string) bool {
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/json", "application/ld+json", "application/xml", "application/rss+xml", "application/atom+xml", "application/javascript", "application/x-yaml", "application/yaml", "application/toml":
		return true
	}
	return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
}

func describeError(err error, ctx context.Context) error {
	var fetchErr *Error
	switch {
	case errors.As(err, &fetchErr):
		return fetchErr
	case errors.Is(err, errBlockedAddress):
		return failf("that address points to a private or local network and can't be read")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failf("the site took too long to answer")
	case errors.Is(ctx.Err(), context.Canceled):
		return ctx.Err()
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return failf("the site's address could not be found")
	}
	return failf("the site could not be reached")
}
