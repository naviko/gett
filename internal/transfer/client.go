package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gett/internal/errs"
	"gett/internal/httpx"
)

// newClient talks to sharers directly: no proxy, no redirects, short
// connect timeout, enough idle connections for every unit worker.
func newClient() *http.Client {
	t := httpx.Transport()
	t.Proxy = nil
	t.ForceAttemptHTTP2 = false
	t.DialContext = (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = 10 * time.Second
	t.MaxIdleConnsPerHost = 32
	return &http.Client{
		Transport:     t,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// peer is a sharer as the receiver talks to it.
type peer struct {
	Name, Addr string
	Port       int
	Passwords  []string
	client     *http.Client
}

func (p *peer) hostPort() string { return net.JoinHostPort(p.Addr, strconv.Itoa(p.Port)) }

// apiError is a reply the sharer refused. The caller knows what the
// request meant and says so.
type apiError struct {
	Code int
	Body string
}

func (e *apiError) Error() string { return "sharer answered " + strconv.Itoa(e.Code) + " " + e.Body }

// status is the sharer's status code behind err, or 0.
func status(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return 0
}

// do sends one request with everything a sharer expects. A connection
// failure names the sharer; a refused request comes back as apiError.
func (p *peer) do(ctx context.Context, method, path string, q url.Values, body io.Reader, hdr http.Header) (*http.Response, error) {
	u := "http://" + p.hostPort() + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := httpx.NewRequest(ctx, method, u, body, hdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Gett-Api", strconv.Itoa(api))
	req.Header.Set("Gett-Name", hostName())
	for _, pw := range p.Passwords {
		req.Header.Add("Gett-Password", pw)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &errs.Peer{Name: p.Name, Addr: p.hostPort(), Err: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	resp.Body.Close()
	text := strings.TrimSpace(string(b))
	if resp.StatusCode == http.StatusUpgradeRequired {
		return nil, fmt.Errorf("%s runs a different gett version; update the older one", p.Name)
	}
	return nil, &apiError{Code: resp.StatusCode, Body: text}
}

func (p *peer) getJSON(ctx context.Context, path string, q url.Values, v any) error {
	resp, err := p.do(ctx, http.MethodGet, path, q, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// ls asks for the children of a wire path, or the shares when it is empty.
func (p *peer) ls(ctx context.Context, path string) ([]item, error) {
	var out struct{ Items []item }
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	if err := p.getJSON(ctx, "/v1/ls", q, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// note tells the sharer a pull of path starts or ends; a failure is nobody's
// problem.
func (p *peer) note(ctx context.Context, path string, done, ok bool) {
	b, _ := json.Marshal(notice{Path: path, OK: ok})
	which := "/v1/start"
	if done {
		which = "/v1/done"
	}
	if resp, err := p.do(ctx, http.MethodPost, which, nil, strings.NewReader(string(b)), nil); err == nil {
		resp.Body.Close()
	}
}
