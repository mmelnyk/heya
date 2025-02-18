// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package requester provides commands to run load tests and display results.
package requester

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"sync"
	"time"

	"go.melnyk.org/mansi"
	"go.melnyk.org/spinner"
	"golang.org/x/net/http2"
)

// Max size of the buffer of result channel.
const maxResult = 32000000
const maxIdleConn = 500

type result struct {
	err           error
	statusCode    int
	offset        time.Duration
	duration      time.Duration
	connDuration  time.Duration // connection setup(DNS lookup + Dial up) duration
	dnsDuration   time.Duration // dns lookup duration
	reqDuration   time.Duration // request "write" duration
	resDuration   time.Duration // response "read" duration
	delayDuration time.Duration // delay between response and request
	contentLength int64
}

type Work struct {
	// Request is the request to be made.
	Request *http.Request

	RequestBody []byte

	// RequestFunc is a function to generate requests. If it is nil, then
	// Request and RequestData are cloned for each request.
	RequestFunc func() *http.Request

	// N is the total number of requests to make.
	N int

	// C is the concurrency level, the number of concurrent workers to run.
	C int

	// K is an option to allow insecure server connections when using TLS.
	K bool

	// H2 is an option to make HTTP/2 requests
	H2 bool

	// TLSResume is used to decide whether TLS session resumption is enabled between requests
	TLSResume bool

	// Timeout in seconds.
	Timeout int

	// Qps is the rate limit in queries per second.
	QPS float64

	// DisableCompression is an option to disable compression in response
	DisableCompression bool

	// DisableKeepAlives is an option to prevents re-use of TCP connections between different HTTP requests
	DisableKeepAlives bool

	// DisableRedirects is an option to prevent the following of HTTP redirects
	DisableRedirects bool

	// Output represents the output type. If "csv" is provided, the
	// output will be dumped as a csv stream.
	Output string

	// ProxyAddr is the address of HTTP proxy server in the format on "host:port".
	// Optional.
	ProxyAddr *url.URL

	// Writer is where results will be written. If nil, results are written to stdout.
	Writer io.Writer

	initOnce sync.Once
	results  chan *result
	start    time.Duration

	report *report
}

func (b *Work) writer() io.Writer {
	if b.Writer == nil {
		return os.Stdout
	}
	return b.Writer
}

// Init initializes internal data-structures
func (b *Work) Init() {
	b.initOnce.Do(func() {
		b.results = make(chan *result, min(b.C*1000, maxResult))
	})
}

// Run makes all the requests, prints the summary. It blocks until
// all work is done.
func (b *Work) Run(ctx context.Context) {
	sctx, cancel := context.WithCancel(ctx)
	spinner := spinner.NewSpinner(spinner.WithStyle(spinner.StyleBars), spinner.WithElapsedTimer())
	fmt.Printf("%s", mansi.ColorTextYellow+mansi.CursorHide)
	spinner.Message("Testing in progress...")
	spinner.Process(sctx)
	b.Init()
	b.start = now()
	b.report = newReport(b.writer(), b.results, b.Output, b.N)
	// Run the reporter first, it polls the result channel until it is closed.
	go func() {
		runReporter(b.report)
	}()
	b.runWorkers(ctx)
	cancel()
	fmt.Printf("\r%s", mansi.LineEraseToEnd+mansi.ResetColor+mansi.CursorShow)
	b.finish()
}

func (b *Work) finish() {
	close(b.results)
	total := now() - b.start
	// Wait until the reporter is done.
	<-b.report.done
	b.report.finalize(total)
}

func (b *Work) makeRequest(ctx context.Context, c *http.Client) {
	var size int64
	var code int
	var dnsStart, connStart, resStart, reqStart, delayStart time.Duration
	var dnsDuration, connDuration, resDuration, reqDuration, delayDuration time.Duration
	var mu sync.Mutex
	var req *http.Request
	if b.RequestFunc != nil {
		req = b.RequestFunc()
	} else {
		req = cloneRequest(b.Request, b.RequestBody)
	}
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			dnsStart = now()
		},
		DNSDone: func(dnsInfo httptrace.DNSDoneInfo) {
			mu.Lock()
			dnsDuration = now() - dnsStart
			mu.Unlock()
		},
		GetConn: func(h string) {
			connStart = now()
		},
		GotConn: func(connInfo httptrace.GotConnInfo) {
			if !connInfo.Reused {
				connDuration = now() - connStart
			}
			reqStart = now()
		},
		WroteRequest: func(w httptrace.WroteRequestInfo) {
			t := now()
			reqDuration = t - reqStart
			mu.Lock()
			delayStart = t
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			t := now()
			mu.Lock()
			delayDuration = t - delayStart
			resStart = t
			mu.Unlock()
		},
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace))
	s := now()
	resp, err := c.Do(req)
	if err == nil {
		code = resp.StatusCode
		size, _ = io.Copy(ioutil.Discard, resp.Body)
		resp.Body.Close()
	}
	t := now()
	resDuration = t - resStart
	finish := t - s
	mu.Lock()
	b.results <- &result{
		offset:        s,
		statusCode:    code,
		duration:      finish,
		err:           err,
		contentLength: size,
		connDuration:  connDuration,
		dnsDuration:   dnsDuration,
		reqDuration:   reqDuration,
		resDuration:   resDuration,
		delayDuration: delayDuration,
	}
	mu.Unlock()
}

func (b *Work) runWorker(ctx context.Context, client *http.Client, n int) {
	var throttle <-chan time.Time
	if b.QPS > 0 {
		throttle = time.Tick(time.Duration(1e6/(b.QPS)) * time.Microsecond)
	}

	if b.DisableRedirects {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	for i := 0; i < n; i++ {
		// Check if application is stopped. Do not send into a closed channel.
		select {
		case <-ctx.Done():
			return
		default:
			if b.QPS > 0 {
				<-throttle
			}
			b.makeRequest(ctx, client)
		}
	}
}

func (b *Work) runWorkers(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(b.C)

	hostName, _, err := net.SplitHostPort(b.Request.Host)
	if err != nil {
		hostName = b.Request.Host
	}

	var tlsCache tls.ClientSessionCache

	if b.TLSResume {
		tlsCache = tls.NewLRUClientSessionCache(1) // we only have one target
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: b.K,
			ServerName:         hostName,
			ClientSessionCache: tlsCache,
		},
		MaxIdleConnsPerHost: min(b.C, maxIdleConn),
		DisableCompression:  b.DisableCompression,
		DisableKeepAlives:   b.DisableKeepAlives,
		Proxy:               http.ProxyURL(b.ProxyAddr),
	}
	if b.H2 {
		http2.ConfigureTransport(tr)
	} else {
		tr.TLSNextProto = make(map[string]func(string, *tls.Conn) http.RoundTripper)
	}
	client := &http.Client{Transport: tr, Timeout: time.Duration(b.Timeout) * time.Second}

	// Ignore the case where b.N % b.C != 0.
	left := b.N
	for i := 0; i < b.C-1; i++ {
		n := left / (b.C - i)
		left = left - n
		go func(n int) {
			b.runWorker(ctx, client, n)
			wg.Done()
		}(n)
	}

	go func(n int) {
		b.runWorker(ctx, client, n)
		wg.Done()
	}(left)

	wg.Wait()
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
func cloneRequest(r *http.Request, body []byte) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header, len(r.Header))
	for k, s := range r.Header {
		r2.Header[k] = append([]string(nil), s...)
	}
	if len(body) > 0 {
		r2.Body = ioutil.NopCloser(bytes.NewReader(body))
	}
	return r2
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
