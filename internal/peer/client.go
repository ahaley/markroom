package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Limits on what a peer can make us swallow. A peer is a machine you chose to
// trust, but trust is not a reason to let it hand you an unbounded response.
const (
	maxManifestBytes = 32 << 20
	maxDocBytes      = 8 << 20
	MaxDocsPerRoot   = 50_000
	MaxRootsPerPeer  = 256
)

// ErrNotModified reports that the peer's manifest is unchanged since the
// ETag we sent, so there is nothing to do.
var ErrNotModified = errors.New("manifest not modified")

// Client fetches manifests and file bodies from a peer. It is the only
// outbound HTTP in markroom.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{
		Timeout: 30 * time.Second,
		// Never follow a peer's redirect: it is an invitation to fetch
		// something on our side of the network on its behalf.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// ValidatePeerURL checks a URL is something we are willing to talk to, and
// returns it trimmed of any trailing slash.
func ValidatePeerURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("peer URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("peer URL %q has no host", raw)
	}
	return strings.TrimRight(u.Scheme+"://"+u.Host+u.Path, "/"), nil
}

// Manifest fetches a peer's manifest. selfID and any other server IDs we
// stand for are announced so the peer can withhold anything that would loop
// back to us. etag may be empty; when it matches, ErrNotModified is returned.
func (c *Client) Manifest(ctx context.Context, base string, seen []string, etag string) (*Manifest, string, error) {
	u := base + ManifestPath
	if len(seen) > 0 {
		u += "?seen=" + url.QueryEscape(strings.Join(seen, ","))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return nil, etag, ErrNotModified
	default:
		return nil, "", fmt.Errorf("manifest: %s: %s", resp.Status, firstLine(resp.Body))
	}

	var m Manifest
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxManifestBytes)).Decode(&m); err != nil {
		return nil, "", fmt.Errorf("decoding manifest: %w", err)
	}
	if m.Protocol != Protocol {
		return nil, "", fmt.Errorf("peer speaks protocol %d, this markroom speaks %d", m.Protocol, Protocol)
	}
	if m.Server.ID == "" {
		return nil, "", errors.New("peer reported no server id")
	}
	if len(m.Roots) > MaxRootsPerPeer {
		return nil, "", fmt.Errorf("peer advertised %d roots, more than the %d limit", len(m.Roots), MaxRootsPerPeer)
	}
	return &m, resp.Header.Get("ETag"), nil
}

// Fetch returns the bytes of one file from a peer's root.
func (c *Client) Fetch(ctx context.Context, base, rootName, relPath string) ([]byte, error) {
	u := base + RawPath + url.PathEscape(rootName) + "/" + escapePath(relPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s/%s: %s", rootName, relPath, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDocBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDocBytes {
		return nil, fmt.Errorf("fetch %s/%s: larger than the %d byte limit", rootName, relPath, maxDocBytes)
	}
	return body, nil
}

// escapePath escapes each segment of a slash-separated path, leaving the
// separators intact so the peer's mux still sees the right shape.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// firstLine reads a short prefix of an error body so a peer's own complaint
// (a host-guard refusal, say) reaches the operator instead of just a code.
func firstLine(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 512))
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
