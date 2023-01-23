package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type HostRewriter struct {
	host string
	next http.RoundTripper
}

func NewHostRewriter(host string, next http.RoundTripper) HostRewriter {
	return HostRewriter{
		host: host,
		next: next,
	}
}

func (rt HostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	urlStr := strings.Replace(req.URL.String(), req.Host, rt.host, 1)
	req.URL, _ = url.Parse(urlStr)

	fmt.Println("[PROXY] Rewriting host to", rt.host, "from", req.Host, "["+req.URL.String() + "]")

	req.Host = rt.host
	req.URL.Scheme = "http"

	return rt.next.RoundTrip(req)
}
