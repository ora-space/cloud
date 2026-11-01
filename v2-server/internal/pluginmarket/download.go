package pluginmarket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"golang.org/x/net/proxy"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var ErrPackage = errors.New("plugin package download or digest verification failed")

// ErrTransient reports a package download failure that may succeed on retry.
var ErrTransient = errors.New("transient plugin package download failure")

func packageURL(locator string, r Retrieval) (*url.URL, bool, error) {
	if e := validateLocator(locator); e != nil {
		return nil, false, ErrPackage
	}
	if !strings.Contains(locator, "://") {
		if r.Mode != "s3_sigv4" || strings.ContainsAny(locator, "\\?#") {
			return nil, false, ErrPackage
		}
		u, e := url.Parse(r.Endpoint)
		if e != nil {
			return nil, false, ErrPackage
		}
		u.Path = "/" + r.Bucket + "/" + locator
		return u, true, nil
	}
	u, e := url.Parse(locator)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return nil, false, ErrPackage
	}
	ep, _ := url.Parse(r.Endpoint)
	signed := r.Mode == "s3_sigv4" && ep != nil && strings.EqualFold(ep.Host, u.Host) && u.RawQuery == "" && strings.HasPrefix(u.Path, "/"+r.Bucket+"/")
	for _, part := range strings.Split(u.Path, "/") {
		if part == ".." || part == "." {
			return nil, false, ErrPackage
		}
	}
	return u, signed, nil
}

// Resolve and dial the same public address, including every redirected request. A configured
// SOCKS proxy receives the pinned numeric address rather than the hostname, so it cannot resolve
// the request to a different, non-public endpoint.
func dialPublic(ctx context.Context, network, address string, useProxy bool) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrSource
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil || len(ips) == 0 {
		return nil, ErrTransient
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, ErrSource
		}
	}
	dial := (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	if useProxy {
		raw, e := sourceProxy()
		if e != nil {
			return nil, e
		}
		u, _ := url.Parse(raw)
		forward, e := proxy.SOCKS5("tcp", u.Host, nil, &net.Dialer{Timeout: 15 * time.Second})
		if e != nil {
			return nil, ErrTransient
		}
		d, ok := forward.(proxy.ContextDialer)
		if !ok {
			return nil, ErrTransient
		}
		dial = d.DialContext
	}
	conn, e := dial(ctx, network, net.JoinHostPort(ips[0].Unmap().String(), port))
	if e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransient
	}
	return conn, nil
}

// Download stages a bounded, verified archive; callers own the returned temporary file.
func Download(ctx context.Context, locator, digest string, r Retrieval, credentials Credentials) (string, error) {
	if r.Validate() != nil || !sha256Pattern.MatchString(digest) {
		return "", ErrPackage
	}
	u, signed, e := packageURL(locator, r)
	if e != nil {
		return "", e
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublic(ctx, network, address, r.UseProxy)
		},
		DisableCompression:  true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	defer tr.CloseIdleConnections()
	client := http.Client{Transport: tr, Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if signed || len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil {
			return ErrPackage
		}
		return nil
	}}
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return "", ErrPackage
	}
	if signed {
		if credentials.AccessKey == "" || credentials.SecretKey == "" {
			return "", ErrCredentials
		}
		empty := fmt.Sprintf("%x", sha256.Sum256(nil))
		req.Header.Set("X-Amz-Content-Sha256", empty)
		if e := v4.NewSigner().SignHTTP(ctx, aws.Credentials{AccessKeyID: credentials.AccessKey, SecretAccessKey: credentials.SecretKey}, req, empty, "s3", r.Region, time.Now().UTC(), func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }); e != nil {
			return "", ErrPackage
		}
	}
	resp, e := client.Do(req)
	if e != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(e, ErrSource) {
			return "", ErrSource
		}
		return "", ErrTransient
	}
	defer resp.Body.Close()
	const limit = 256 << 20
	if resp.StatusCode != http.StatusOK {
		return "", packageStatusError(resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return "", ErrPackage
	}
	f, e := os.CreateTemp("", "ora-skill-*.orax")
	if e != nil {
		return "", ErrPackage
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(f.Name())
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", ErrTransient
	}
	if closeErr != nil || n > limit || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), digest) {
		os.Remove(f.Name())
		return "", ErrPackage
	}
	return f.Name(), nil
}

func packageStatusError(status int) error {
	if status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500 {
		return ErrTransient
	}
	return ErrPackage
}
