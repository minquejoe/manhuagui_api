package service

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Aoi-hosizora/ahlib/xconstant/headers"
	"github.com/Aoi-hosizora/ahlib/xmodule"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/module/sn"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/static"
	"github.com/Aoi-hosizora/manhuagui-api/internal/pkg/xrayproxy"
	"github.com/PuerkitoBio/goquery"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"
)

type HttpService struct {
	proxy     *xrayproxy.Manager
	transport *http.Transport
}

func NewHttpService() *HttpService {
	h := &HttpService{
		proxy: xmodule.MustGetByName(sn.SProxyManager).(*xrayproxy.Manager),
	}
	h.transport = &http.Transport{
		Proxy: h.proxyFunc,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
	}
	return h
}

// proxyFunc routes requests to proxied hosts (manhuagui/hamreus by default)
// through the local xray SOCKS proxy when it is ready; everything else goes
// direct. Falls back to direct when xray is not ready yet.
func (h *HttpService) proxyFunc(req *http.Request) (*url.URL, error) {
	if h.proxy != nil && h.proxy.IsReady() && h.proxy.ShouldProxy(req.URL.Hostname()) {
		return h.proxy.ProxyURL(), nil
	}
	return nil, nil
}

func (h *HttpService) DoRequest(client *http.Client, req *http.Request) ([]byte, *http.Response, error) {
	if req.Header.Get(headers.UserAgent) == "" {
		req.Header.Add(headers.UserAgent, static.USER_AGENT)
	}
	if req.Header.Get(headers.Referer) == "" {
		req.Header.Add(headers.Referer, static.REFERER)
	}

	resp, err := client.Do(req)
	if err != nil {
		// Retry idempotent requests once on network errors: when the proxied
		// node dies between probe cycles, the first attempt fails and the
		// retry is routed through the next lowest-latency node.
		if (req.Method == http.MethodGet || req.Method == http.MethodHead) && req.Body == nil {
			resp, err = client.Do(req.Clone(req.Context()))
		}
		if err != nil {
			return nil, nil, fmt.Errorf("network error: %v", err)
		}
	}
	body := resp.Body
	defer body.Close()

	bs, err := io.ReadAll(body)
	if err != nil {
		return nil, nil, fmt.Errorf("response error: %v", err)
	}
	return bs, resp, err
}

func (h *HttpService) HttpGet(url string, fn func(r *http.Request)) ([]byte, *http.Response, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, nil, err
	}
	if fn != nil {
		fn(req)
	}

	return h.DoRequest(&http.Client{Transport: h.transport}, req)
}

// HttpGetStream issues a GET request and returns the response without reading
// the body (the caller must close resp.Body), for streaming large payloads
// such as images. redirectAllowed, when non-nil, validates the host of every
// redirect hop to prevent SSRF. The user agent and referer headers are applied
// like DoRequest, and the request goes through the xray proxy when enabled.
func (h *HttpService) HttpGetStream(rawURL string, redirectAllowed func(host string) bool, fn func(r *http.Request)) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	if fn != nil {
		fn(req)
	}
	if req.Header.Get(headers.UserAgent) == "" {
		req.Header.Add(headers.UserAgent, static.USER_AGENT)
	}
	if req.Header.Get(headers.Referer) == "" {
		req.Header.Add(headers.Referer, static.REFERER)
	}

	client := &http.Client{
		Transport: h.transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if redirectAllowed != nil && !redirectAllowed(req.URL.Hostname()) {
				return fmt.Errorf("redirect to non-allowed host %s", req.URL.Hostname())
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network error: %v", err)
	}
	return resp, nil
}

func (h *HttpService) HttpGetDocument(url string, fn func(*http.Request)) ([]byte, *goquery.Document, error) {
	bs, _, err := h.HttpGet(url, fn)
	if err != nil {
		return nil, nil, err
	}
	if bytes.Contains(bs, []byte(static.NOT_FOUND_TOKEN)) {
		return nil, nil, nil
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(bs))
	if err != nil {
		return nil, nil, fmt.Errorf("document error: %v", err)
	}
	return bs, doc, nil
}

func (h *HttpService) HttpHeadNoRedirect(url string, fn func(r *http.Request)) (*http.Response, error) {
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		return nil, err
	}
	if fn != nil {
		fn(req)
	}

	client := &http.Client{
		Transport: h.transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	_, resp, err := h.DoRequest(client, req)
	return resp, err
}

func (h *HttpService) HttpPost(url string, body io.Reader, fn func(r *http.Request)) ([]byte, *http.Response, error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, nil, err
	}
	if fn != nil {
		fn(req)
	}

	return h.DoRequest(&http.Client{Transport: h.transport}, req)
}
