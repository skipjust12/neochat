package webfetch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestBlocked(t *testing.T) {
	for _, addr := range []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.64.0.1", "0.0.0.0",
		"0.1.2.3", "198.18.0.1", "224.0.0.1", "255.255.255.255", "240.0.0.1",
		"::1", "::", "fe80::1", "fc00::1", "fd12::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1",
		"64:ff9b::a00:1", "2002:7f00:1::", "2001:db8::1", "2001::1",
	} {
		if !Blocked(netip.MustParseAddr(addr)) {
			t.Errorf("%s is not blocked", addr)
		}
	}
	for _, addr := range []string{"8.8.8.8", "93.184.216.34", "1.1.1.1", "2a00:1450:4010:c05::64", "64:ff9b::808:808", "2002:808:808::"} {
		if Blocked(netip.MustParseAddr(addr)) {
			t.Errorf("public %s is blocked", addr)
		}
	}
	if checkDialAddress("10.0.0.1:443") == nil || checkDialAddress("[::1]:80") == nil || checkDialAddress("garbage") == nil {
		t.Error("dial check let a private address through")
	}
	if err := checkDialAddress("8.8.8.8:443"); err != nil {
		t.Errorf("dial check refused a public address: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	good := map[string]string{
		"https://example.com/a?b=1#frag": "https://example.com/a?b=1",
		" HTTP://Example.com:443/x ":     "http://Example.com:443/x",
		"https://example.com:80":         "https://example.com:80",
	}
	for in, want := range good {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("NormalizeURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"ftp://example.com", "file:///etc/passwd", "javascript:alert(1)", "https://user:pw@example.com", "https://example.com:8080/", "example.com", "", "http://"} {
		if _, err := NormalizeURL(in); err == nil {
			t.Errorf("NormalizeURL(%q) accepted", in)
		}
	}
}

func TestFetchRefusesPrivateAddresses(t *testing.T) {
	f := &Fetcher{Timeout: 5 * time.Second}
	for _, target := range []string{"http://127.0.0.1/", "http://[::1]/", "http://localhost/", "http://169.254.169.254/latest/meta-data/"} {
		_, err := f.Fetch(context.Background(), target)
		var fetchErr *Error
		if !errors.As(err, &fetchErr) || !strings.Contains(fetchErr.Message, "private or local network") {
			t.Errorf("%s: err = %v, want a private-network refusal", target, err)
		}
	}
}

const articlePage = `<!doctype html><html><head><title>Release notes — Example</title>
<style>.x{color:red}</style><script>var secret = "do not read";</script></head>
<body>
<nav><a href="/">Home</a> <a href="/pricing">Pricing</a></nav>
<main>
<h1>Version 2.0</h1>
<p>We shipped <b>two</b> things   and
fixed a bug. See <a href="/docs/v2#intro">the docs</a> or <a href="mailto:a@b.c">mail us</a>.</p>
<ul><li>Faster sync</li><li>Dark mode</li></ul>
<table><tr><th>Plan</th><th>Price</th></tr><tr><td>Pro</td><td>$4</td></tr></table>
<pre><code>go get example.com/v2
  indented line</code></pre>
<p hidden>hidden text</p><div style="display: none">invisible</div><div aria-hidden="true">decor</div>
<form><input value="typed"><button>Send</button></form>
</main>
<footer>Copyright footer</footer>
</body></html>`

func TestFetchExtractsReadableText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/old":
			http.Redirect(w, r, "/notes", http.StatusFound)
		case "/notes":
			if !strings.Contains(r.UserAgent(), "NeoChat") {
				t.Errorf("user agent = %q", r.UserAgent())
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, articlePage)
		}
	}))
	defer srv.Close()
	f := &Fetcher{AllowPrivate: true}
	page, err := f.Fetch(context.Background(), srv.URL+"/old#top")
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Release notes — Example" || page.URL != srv.URL+"/old" || page.FinalURL != srv.URL+"/notes" || page.Truncated {
		t.Fatalf("page = %+v", page)
	}
	for _, want := range []string{
		"# Version 2.0",
		"We shipped two things and fixed a bug. See [the docs](" + srv.URL + "/docs/v2#intro) or mail us.",
		"- Faster sync\n- Dark mode",
		"Plan | Price\nPro | $4",
		"```\ngo get example.com/v2\n  indented line\n```",
	} {
		if !strings.Contains(page.Text, want) {
			t.Errorf("text lacks %q:\n%s", want, page.Text)
		}
	}
	for _, unwanted := range []string{"secret", "color:red", "Home", "Pricing", "hidden text", "invisible", "decor", "typed", "Send", "Copyright", "mailto"} {
		if strings.Contains(page.Text, unwanted) {
			t.Errorf("text contains %q:\n%s", unwanted, page.Text)
		}
	}
}

func TestFetchDecodesLegacyCharsetsAndText(t *testing.T) {
	// "Привет, мир" in windows-1251, declared only in a <meta> tag.
	cp1251 := []byte{0xcf, 0xf0, 0xe8, 0xe2, 0xe5, 0xf2, 0x2c, 0x20, 0xec, 0xe8, 0xf0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ru":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte(`<html><head><meta charset="windows-1251"><title>t</title></head><body><p>`))
			w.Write(cp1251)
			w.Write([]byte(`</p><p>` + strings.Repeat("x", 10) + `</p></body></html>`))
		case "/plain":
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "line one\r\n\r\n\r\n\r\nline two   \n")
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	f := &Fetcher{AllowPrivate: true}
	page, err := f.Fetch(context.Background(), srv.URL+"/ru")
	if err != nil || !strings.Contains(page.Text, "Привет, мир") {
		t.Fatalf("cp1251 page = %q, %v", page.Text, err)
	}
	page, err = f.Fetch(context.Background(), srv.URL+"/plain")
	if err != nil || page.Text != "line one\n\nline two" || page.Title != "" {
		t.Fatalf("plain page = %q, %v", page.Text, err)
	}
	page, err = f.Fetch(context.Background(), srv.URL+"/data.json")
	if err != nil || page.Text != `{"ok":true}` {
		t.Fatalf("json page = %q, %v", page.Text, err)
	}
}

func TestFetchFailuresAreExplained(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/missing":
			http.NotFound(w, r)
		case "/doc.pdf":
			w.Header().Set("Content-Type", "application/pdf")
			w.Write([]byte("%PDF-1.7"))
		case "/image":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG"))
		case "/loop":
			hops++
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/js":
			fmt.Fprint(w, `<html><body><div id="root"></div><script>render()</script></body></html>`)
		case "/ftp":
			http.Redirect(w, r, "ftp://example.com/file", http.StatusFound)
		case "/slow":
			time.Sleep(300 * time.Millisecond)
		}
	}))
	defer srv.Close()
	f := &Fetcher{AllowPrivate: true, Timeout: 100 * time.Millisecond}
	for path, want := range map[string]string{
		"/missing": "HTTP 404",
		"/doc.pdf": "PDF documents can't be read",
		"/image":   "image/png",
		"/loop":    "too many redirects",
		"/js":      "no readable text",
		"/ftp":     "only http and https",
		"/slow":    "took too long",
	} {
		_, err := f.Fetch(context.Background(), srv.URL+path)
		var fetchErr *Error
		if !errors.As(err, &fetchErr) || !strings.Contains(fetchErr.Message, want) {
			t.Errorf("%s: err = %v, want %q", path, err, want)
		}
	}
	if hops > maxRedirects+1 {
		t.Errorf("followed %d redirects", hops)
	}
	if _, err := f.Fetch(context.Background(), "http://does-not-exist.invalid/"); err == nil || !strings.Contains(err.Error(), "could not be found") {
		t.Errorf("unknown host: err = %v", err)
	}
}

func TestFetchLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, strings.Repeat("ж", 5000))
	}))
	defer srv.Close()
	page, err := (&Fetcher{AllowPrivate: true, MaxChars: 1000}).Fetch(context.Background(), srv.URL)
	if err != nil || len([]rune(page.Text)) != 1000 || !page.Truncated {
		t.Fatalf("char cap: %d runes, truncated=%v, %v", len([]rune(page.Text)), page.Truncated, err)
	}
	page, err = (&Fetcher{AllowPrivate: true, MaxBytes: 1001}).Fetch(context.Background(), srv.URL)
	if err != nil || !page.Truncated || len(page.Text) > 1001 || strings.ContainsRune(page.Text, '\uFFFD') {
		t.Fatalf("byte cap: %d bytes, truncated=%v, %v", len(page.Text), page.Truncated, err)
	}
}
