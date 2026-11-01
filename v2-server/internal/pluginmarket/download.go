package pluginmarket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"io"
	"net"
	"net/http"

	"net/url"
	"os"
	"strings"
	"time"
)

var ErrPackage = errors.New("plugin package download or digest verification failed")

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

// Resolve and dial the same public address, including every redirected request.
func dialPublic(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, ErrSource
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil || len(ips) == 0 {
		return nil, ErrSource
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, ErrSource
		}
	}
	return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
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
	tr := &http.Transport{DialContext: dialPublic, DisableCompression: true, TLSHandshakeTimeout: 15 * time.Second}
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
		return "", ErrPackage
	}
	defer resp.Body.Close()
	const limit = 256 << 20
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit {
		return "", ErrPackage
	}
	f, e := os.CreateTemp("", "ora-skill-*.orax")
	if e != nil {
		return "", ErrPackage
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || n > limit || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), digest) {
		os.Remove(f.Name())
		return "", ErrPackage
	}
	return f.Name(), nil
}
