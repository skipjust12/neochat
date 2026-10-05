// Package imagegen makes pictures and videos through Polza's Media API:
// POST /v1/media starts a generation, GET /v1/media/{id} is polled until
// it's done, and the result is downloaded right away -- Polza keeps it for
// only seven days, while a picture or a video in a chat has to last as long
// as the chat (server/image.go and server/video.go store them with the
// chat's files).
//
// Requests run on the user's own image key, sent per call and never kept.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	_ "golang.org/x/image/webp"

	"neochat/provider"
	"neochat/webfetch"
)

const (
	defaultBaseURL       = "https://polza.ai/api/v1"
	defaultPollInterval  = 3 * time.Second
	defaultMaxWait       = 4 * time.Minute
	defaultMaxImageBytes = 25 << 20
	callTimeout          = 90 * time.Second
	maxStatusBytes       = 4 << 20
	maxImages            = 4
)

// Client talks to the Media API. The zero value is ready to use.
type Client struct {
	// BaseURL is the API root; empty means Polza's.
	BaseURL string
	// PollInterval is how often a running generation is checked.
	PollInterval time.Duration
	// MaxWait bounds a whole generation, polling included.
	MaxWait time.Duration
	// MaxImageBytes caps one downloaded picture.
	MaxImageBytes int64
	// MaxVideoWait bounds a whole video generation; MaxVideoBytes caps the
	// downloaded clip.
	MaxVideoWait  time.Duration
	MaxVideoBytes int64
	// AllowPrivate lets results be downloaded from private addresses and
	// over plain HTTP. Tests only: in production a result URL pointing
	// inside the network is refused.
	AllowPrivate bool
	// HTTP makes the API calls (not the downloads); nil means a default
	// client.
	HTTP *http.Client
}

// Reference is an input picture: one to edit, or to take after.
type Reference struct {
	MIME string
	Data []byte
}

// Request is one generation.
type Request struct {
	Model       string
	Prompt      string
	AspectRatio string // optional, e.g. "1:1"; empty lets the model pick
	References  []Reference
	User        string // end-user id, for Polza's abuse tracking
}

// Image is a generated picture, downloaded.
type Image struct {
	MIME          string
	Data          []byte
	Width, Height int
}

// Result is what a finished generation produced.
type Result struct {
	Images []Image
	// Text is what the model wrote with (or instead of) the picture.
	Text string
	// CostRUB is what Polza charged, when it said; HasCost tells a free
	// generation from a missing figure.
	CostRUB float64
	HasCost bool
}

// ErrMissingKey means the request carried no image key.
var ErrMissingKey = errors.New("imagegen: no image API key")

// FailedError is a generation Polza accepted and then reported as failed
// (content policy, a provider error). Message is Polza's own explanation,
// meant for the user.
type FailedError struct {
	Code    string
	Message string
}

func (e *FailedError) Error() string {
	return "imagegen: generation failed: " + e.Code + ": " + e.Message
}

// TimeoutError means the generation was still running at MaxWait.
type TimeoutError struct{ ID string }

func (e *TimeoutError) Error() string {
	return "imagegen: generation " + e.ID + " is still running"
}

type mediaStatus struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Data    json.RawMessage `json:"data"`
	Output  json.RawMessage `json:"output"`
	Content string          `json:"content"`
	Usage   *struct {
		CostRUB *float64 `json:"cost_rub"`
		Cost    *float64 `json:"cost"`
	} `json:"usage"`
	Error json.RawMessage `json:"error"`
}

// Generate runs one generation to the end and downloads what it made.
func (c *Client) Generate(ctx context.Context, apiKey string, req Request) (Result, error) {
	if strings.TrimSpace(apiKey) == "" {
		return Result{}, ErrMissingKey
	}
	input := map[string]any{"prompt": req.Prompt}
	if req.AspectRatio != "" {
		input["aspect_ratio"] = req.AspectRatio
	}
	if len(req.References) > 0 {
		input["images"] = inlineFiles(req.References)
	}
	body := map[string]any{"model": req.Model, "input": input}
	if req.User != "" {
		body["user"] = req.User
	}

	maxWait := c.MaxWait
	if maxWait <= 0 {
		maxWait = defaultMaxWait
	}
	status, err := c.run(ctx, apiKey, body, maxWait)
	if err != nil {
		return Result{}, err
	}

	result := Result{Text: strings.TrimSpace(status.Content)}
	result.CostRUB, result.HasCost = status.cost()
	var found results
	found.collect(status.Data, 0)
	found.collect(status.Output, 0)
	for _, data := range found.inline {
		if len(result.Images) == maxImages {
			break
		}
		picture, err := decodeImage(data)
		if err != nil {
			return result, err
		}
		result.Images = append(result.Images, picture)
	}
	for _, link := range found.urls {
		if len(result.Images) == maxImages {
			break
		}
		data, err := c.download(ctx, link, c.maxImageBytes())
		if err != nil {
			return result, err
		}
		picture, err := decodeImage(data)
		if err != nil {
			return result, err
		}
		result.Images = append(result.Images, picture)
	}
	if len(result.Images) == 0 && result.Text == "" {
		return result, errors.New("imagegen: the generation finished without an image")
	}
	return result, nil
}

// inlineFiles is the Media API's form for files sent with a request:
// base64 data URLs (Polza moves them to its storage when a provider wants
// a link).
func inlineFiles(refs []Reference) []map[string]string {
	out := make([]map[string]string, len(refs))
	for i, ref := range refs {
		out[i] = map[string]string{"type": "base64", "data": "data:" + ref.MIME + ";base64," + base64.StdEncoding.EncodeToString(ref.Data)}
	}
	return out
}

func (s mediaStatus) cost() (float64, bool) {
	if s.Usage == nil {
		return 0, false
	}
	switch {
	case s.Usage.CostRUB != nil:
		return *s.Usage.CostRUB, true
	case s.Usage.Cost != nil:
		return *s.Usage.Cost, true
	}
	return 0, false
}

// run starts a generation and polls it until it's done, for at most
// maxWait; a failed one is a *FailedError, one still running a
// *TimeoutError.
func (c *Client) run(ctx context.Context, apiKey string, body map[string]any, maxWait time.Duration) (mediaStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, maxWait)
	defer cancel()

	status, err := c.call(ctx, apiKey, http.MethodPost, "/media", body)
	poll := c.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	id := status.ID
	for err == nil && (status.Status == "pending" || status.Status == "processing") {
		if id == "" {
			return mediaStatus{}, errors.New("imagegen: running generation has no id")
		}
		select {
		case <-ctx.Done():
		case <-time.After(poll):
			status, err = c.call(ctx, apiKey, http.MethodGet, "/media/"+url.PathEscape(id), nil)
			continue
		}
		err = ctx.Err()
	}
	if err != nil {
		if id != "" && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return mediaStatus{}, &TimeoutError{ID: id}
		}
		return mediaStatus{}, err
	}

	switch status.Status {
	case "completed":
		return status, nil
	case "failed", "cancelled":
		return mediaStatus{}, failure(status)
	}
	return mediaStatus{}, fmt.Errorf("imagegen: unexpected status %q", status.Status)
}

func (c *Client) maxImageBytes() int64 {
	if c.MaxImageBytes > 0 {
		return c.MaxImageBytes
	}
	return defaultMaxImageBytes
}

func failure(status mediaStatus) error {
	failed := &FailedError{Code: status.Status}
	var detail struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		Metadata struct {
			Raw string `json:"raw"`
		} `json:"metadata"`
	}
	var text string
	switch {
	case json.Unmarshal(status.Error, &detail) == nil && (detail.Code != "" || detail.Message != ""):
		if detail.Code != "" {
			failed.Code = detail.Code
		}
		failed.Message = strings.TrimSpace(detail.Message)
		if raw := strings.TrimSpace(detail.Metadata.Raw); raw != "" && !strings.Contains(failed.Message, raw) {
			failed.Message = strings.TrimSpace(failed.Message + " (" + raw + ")")
		}
	case json.Unmarshal(status.Error, &text) == nil:
		failed.Message = strings.TrimSpace(text)
	}
	if failed.Message == "" {
		failed.Message = strings.TrimSpace(status.Content)
	}
	return failed
}

func (c *Client) call(ctx context.Context, apiKey, method, path string, body any) (mediaStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return mediaStatus{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+path, reader)
	if err != nil {
		return mediaStatus{}, err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return mediaStatus{}, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxStatusBytes))
	if err != nil {
		return mediaStatus{}, err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return mediaStatus{}, &provider.StatusError{StatusCode: response.StatusCode, Body: string(data)}
	}
	var status mediaStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return mediaStatus{}, fmt.Errorf("imagegen: decode %s %s: %w", method, path, err)
	}
	return status, nil
}

// results gathers what a finished generation points at. The docs show the
// picture under data.url, under output.url and as lists of either, so any
// http(s) string under an URL-ish key counts, as does an inline base64 one.
type results struct {
	urls   []string
	inline [][]byte
	seen   map[string]bool
}

var urlKeys = map[string]bool{"url": true, "urls": true, "image_url": true, "image": true, "images": true, "src": true, "uri": true, "video": true, "videos": true, "video_url": true}
var base64Keys = map[string]bool{"b64_json": true, "base64": true, "b64": true}

func (r *results) collect(raw json.RawMessage, depth int) {
	if len(raw) == 0 || depth > 6 {
		return
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return
	}
	r.walk("", value, depth)
}

func (r *results) walk(key string, value any, depth int) {
	if depth > 6 {
		return
	}
	switch v := value.(type) {
	case string:
		r.add(key, v)
	case []any:
		for _, item := range v {
			r.walk(key, item, depth+1)
		}
	case map[string]any:
		for k, item := range v {
			r.walk(strings.ToLower(k), item, depth+1)
		}
	}
}

func (r *results) add(key, value string) {
	value = strings.TrimSpace(value)
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if value == "" || r.seen[value] {
		return
	}
	if strings.HasPrefix(value, "data:image/") {
		if comma := strings.Index(value, ","); comma > 0 && strings.Contains(value[:comma], ";base64") {
			if data, err := base64.StdEncoding.DecodeString(value[comma+1:]); err == nil {
				r.seen[value] = true
				r.inline = append(r.inline, data)
			}
		}
		return
	}
	if base64Keys[key] {
		if data, err := base64.StdEncoding.DecodeString(value); err == nil {
			r.seen[value] = true
			r.inline = append(r.inline, data)
		}
		return
	}
	if urlKeys[key] && (strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")) {
		r.seen[value] = true
		r.urls = append(r.urls, value)
	}
}

// download fetches a result, at most limit bytes. Polza serves results
// from its own storage, but the address still comes from a response, so it
// gets the same treatment as any outside URL: HTTPS, public addresses
// only, a size cap.
func (c *Client) download(ctx context.Context, link string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(link)
	if err != nil || (parsed.Scheme != "https" && !(c.AllowPrivate && parsed.Scheme == "http")) {
		return nil, fmt.Errorf("imagegen: refusing to download %q", link)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if !c.AllowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return errors.New("imagegen: bad address")
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || webfetch.Blocked(ip) {
				return errors.New("imagegen: result is on a private or local address")
			}
			return nil
		}
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 15 * time.Second, Proxy: nil},
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("imagegen: too many redirects")
			}
			if next.URL.Scheme != "https" && !c.AllowPrivate {
				return errors.New("imagegen: redirect away from https")
			}
			return nil
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("imagegen: download result: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imagegen: download result: HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("imagegen: download result: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("imagegen: result is larger than %d bytes", limit)
	}
	return data, nil
}

// decodeImage checks the bytes are a picture and reads its size.
func decodeImage(data []byte) (Image, error) {
	mime := http.DetectContentType(data)
	switch mime {
	case "image/png", "image/jpeg", "image/webp", "image/gif":
	default:
		return Image{}, fmt.Errorf("imagegen: result is %s, not an image", mime)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, fmt.Errorf("imagegen: read result: %w", err)
	}
	return Image{MIME: mime, Data: data, Width: config.Width, Height: config.Height}, nil
}
