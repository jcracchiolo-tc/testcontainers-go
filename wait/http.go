package wait

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/docker/go-connections/nat"
)

// Implement interface
var (
	_ Strategy        = (*HTTPStrategy)(nil)
	_ StrategyTimeout = (*HTTPStrategy)(nil)
)

type HTTPStrategy struct {
	// all Strategies should have a startupTimeout to avoid waiting infinitely
	timeout *time.Duration

	// additional properties
	Port              nat.Port
	Path              string
	StatusCodeMatcher func(status int) bool
	ResponseMatcher   func(body io.Reader) bool
	UseTLS            bool
	AllowInsecure     bool
	TLSConfig         *tls.Config // TLS config for HTTPS
	Method            string      // http method
	Body              io.Reader   // http request body
	PollInterval      time.Duration
	UserInfo          *url.Userinfo
}

// NewHTTPStrategy constructs a HTTP strategy waiting on port 80 and status code 200
func NewHTTPStrategy(path string) *HTTPStrategy {
	return &HTTPStrategy{
		Port:              "",
		Path:              path,
		StatusCodeMatcher: defaultStatusCodeMatcher,
		ResponseMatcher:   func(body io.Reader) bool { return true },
		UseTLS:            false,
		TLSConfig:         nil,
		Method:            http.MethodGet,
		Body:              nil,
		PollInterval:      defaultPollInterval(),
		UserInfo:          nil,
	}
}

func defaultStatusCodeMatcher(status int) bool {
	return status == http.StatusOK
}

// fluent builders for each property
// since go has neither covariance nor generics, the return type must be the type of the concrete implementation
// this is true for all properties, even the "shared" ones like startupTimeout

// WithStartupTimeout can be used to change the default startup timeout
func (ws *HTTPStrategy) WithStartupTimeout(timeout time.Duration) *HTTPStrategy {
	ws.timeout = &timeout
	return ws
}

func (ws *HTTPStrategy) WithPort(port nat.Port) *HTTPStrategy {
	ws.Port = port
	return ws
}

func (ws *HTTPStrategy) WithStatusCodeMatcher(statusCodeMatcher func(status int) bool) *HTTPStrategy {
	ws.StatusCodeMatcher = statusCodeMatcher
	return ws
}

func (ws *HTTPStrategy) WithResponseMatcher(matcher func(body io.Reader) bool) *HTTPStrategy {
	ws.ResponseMatcher = matcher
	return ws
}

func (ws *HTTPStrategy) WithTLS(useTLS bool, tlsconf ...*tls.Config) *HTTPStrategy {
	ws.UseTLS = useTLS
	if useTLS && len(tlsconf) > 0 {
		ws.TLSConfig = tlsconf[0]
	}
	return ws
}

func (ws *HTTPStrategy) WithAllowInsecure(allowInsecure bool) *HTTPStrategy {
	ws.AllowInsecure = allowInsecure
	return ws
}

func (ws *HTTPStrategy) WithMethod(method string) *HTTPStrategy {
	ws.Method = method
	return ws
}

func (ws *HTTPStrategy) WithBody(reqdata io.Reader) *HTTPStrategy {
	ws.Body = reqdata
	return ws
}

func (ws *HTTPStrategy) WithBasicAuth(username, password string) *HTTPStrategy {
	ws.UserInfo = url.UserPassword(username, password)
	return ws
}

// WithPollInterval can be used to override the default polling interval of 100 milliseconds
func (ws *HTTPStrategy) WithPollInterval(pollInterval time.Duration) *HTTPStrategy {
	ws.PollInterval = pollInterval
	return ws
}

// ForHTTP is a convenience method similar to Wait.java
// https://github.com/testcontainers/testcontainers-java/blob/1d85a3834bd937f80aad3a4cec249c027f31aeb4/core/src/main/java/org/testcontainers/containers/wait/strategy/Wait.java
func ForHTTP(path string) *HTTPStrategy {
	return NewHTTPStrategy(path)
}

func (ws *HTTPStrategy) Timeout() *time.Duration {
	return ws.timeout
}

// WaitUntilReady implements Strategy.WaitUntilReady
func (ws *HTTPStrategy) WaitUntilReady(ctx context.Context, target StrategyTarget) (err error) {
	timeout := defaultStartupTimeout()
	if ws.timeout != nil {
		timeout = *ws.timeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ipAddress, err := target.Host(ctx)
	if err != nil {
		return
	}

	var mappedPort nat.Port
	if ws.Port == "" {
		ports, err := target.Ports(ctx)
		for err != nil {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", ctx.Err(), err)
			case <-time.After(ws.PollInterval):
				if err := checkTarget(ctx, target); err != nil {
					return err
				}

				ports, err = target.Ports(ctx)
			}
		}

		for k, bindings := range ports {
			if len(bindings) == 0 || k.Proto() != "tcp" {
				continue
			}
			mappedPort, _ = nat.NewPort(k.Proto(), bindings[0].HostPort)
			break
		}

		if mappedPort == "" {
			return errors.New("No exposed tcp ports or mapped ports - cannot wait for status")
		}
	} else {
		mappedPort, err = target.MappedPort(ctx, ws.Port)

		for mappedPort == "" {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", ctx.Err(), err)
			case <-time.After(ws.PollInterval):
				if err := checkTarget(ctx, target); err != nil {
					return err
				}

				mappedPort, err = target.MappedPort(ctx, ws.Port)
			}
		}

		if mappedPort.Proto() != "tcp" {
			return errors.New("Cannot use HTTP client on non-TCP ports")
		}
	}

	switch ws.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPut, http.MethodPatch, http.MethodDelete,
		http.MethodConnect, http.MethodOptions, http.MethodTrace:
	default:
		if ws.Method != "" {
			return fmt.Errorf("invalid http method %q", ws.Method)
		}
		ws.Method = http.MethodGet
	}

	tripper := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       ws.TLSConfig,
	}

	var proto string
	if ws.UseTLS {
		proto = "https"
		if ws.AllowInsecure {
			if ws.TLSConfig == nil {
				tripper.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			} else {
				ws.TLSConfig.InsecureSkipVerify = true
			}
		}
	} else {
		proto = "http"
	}

	client := http.Client{Transport: tripper, Timeout: time.Second}
	address := net.JoinHostPort(ipAddress, strconv.Itoa(mappedPort.Int()))

	endpoint := url.URL{
		Scheme: proto,
		Host:   address,
		Path:   ws.Path,
	}

	if ws.UserInfo != nil {
		endpoint.User = ws.UserInfo
	}

	// cache the body into a byte-slice so that it can be iterated over multiple times
	var body []byte
	if ws.Body != nil {
		body, err = io.ReadAll(ws.Body)
		if err != nil {
			return
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(ws.PollInterval):
			if err := checkTarget(ctx, target); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, ws.Method, endpoint.String(), bytes.NewReader(body))
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			if ws.StatusCodeMatcher != nil && !ws.StatusCodeMatcher(resp.StatusCode) {
				_ = resp.Body.Close()
				continue
			}
			if ws.ResponseMatcher != nil && !ws.ResponseMatcher(resp.Body) {
				_ = resp.Body.Close()
				continue
			}
			if err := resp.Body.Close(); err != nil {
				continue
			}
			return nil
		}
	}
}
