// Package objectstore signs short-lived uploads and verifies stored metadata through private S3.
// Signing is local cryptography; verification performs HEAD outside database transactions.
// Grants are bearer credentials and must not be written to the database or to logs.
package objectstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Config is the in-memory view of Cloud's object store. Credential values live only in this
// process; callers load them from files and never persist them.
type Config struct {
	Endpoint        string
	PublicEndpoint  string
	Region          string
	Bucket          string
	PathStyle       bool
	AccessKeyID     string
	SecretAccessKey string
	UploadGrantTTL  time.Duration
}

// Grant is one presigned PUT. Headers must be sent unchanged; legacy uploaders also add their
// computed checksum. The signed creation condition keeps still-live grants from replacing objects.
type Grant struct {
	URL     string
	Method  string
	Headers map[string]string
	Expires time.Time
}

// PresignPUT signs a single-object PUT that expires after the configured grant TTL.
// The key must be the object key Cloud already fixed for this delivery attempt.
func PresignPUT(cfg *Config, key string, now time.Time) (Grant, error) {
	return presign(cfg, key, "PUT", map[string]string{"x-amz-sdk-checksum-algorithm": "SHA256"}, now)
}

// PresignPUTChecksum binds the Node-computed SHA-256 to the upload capability. S3 must validate
// the bytes against this signed header before recording a checksum Cloud can verify with HEAD.
func PresignPUTChecksum(cfg *Config, key, digest string, now time.Time) (Grant, error) {
	checksum, err := hex.DecodeString(digest)
	if err != nil || len(checksum) != sha256.Size || hex.EncodeToString(checksum) != digest {
		return Grant{}, fmt.Errorf("invalid object checksum")
	}
	return presign(cfg, key, "PUT", map[string]string{"x-amz-sdk-checksum-algorithm": "SHA256", "x-amz-checksum-sha256": base64.StdEncoding.EncodeToString(checksum)}, now)
}

// PresignGET signs a single-object read for the Node that restores a prior Revision. It carries no
// condition: the Node verifies size and SHA-256 itself before Git sees the bytes.
func PresignGET(cfg *Config, key string, now time.Time) (Grant, error) {
	return presign(cfg, key, "GET", map[string]string{}, now)
}

// presign shares canonicalization between the Node-facing PUT and Cloud's private HEAD.
func presign(cfg *Config, key, method string, headers map[string]string, now time.Time) (Grant, error) {
	if cfg == nil || cfg.Endpoint == "" || cfg.Region == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" {
		return Grant{}, fmt.Errorf("object store is not configured")
	}
	if !validKey(key) {
		return Grant{}, fmt.Errorf("invalid object key")
	}
	// A grant can outlive settlement. Bind create-only semantics into every PUT, including legacy
	// grants, so a second capability cannot change an object after Cloud's external verification.
	if method == "PUT" {
		headers["if-none-match"] = "*"
	}
	ttl := cfg.UploadGrantTTL
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl < time.Second || ttl > 7*24*time.Hour {
		return Grant{}, fmt.Errorf("invalid upload grant lifetime")
	}
	base := cfg.Endpoint
	if cfg.PublicEndpoint != "" {
		base = cfg.PublicEndpoint
	}
	endpoint, err := url.Parse(base)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return Grant{}, fmt.Errorf("invalid object store endpoint")
	}
	// SigV4 timestamps and lifetimes have second precision. Report exactly the expiry the
	// server enforces, so Node cannot mistake a fractional second for a usable grant.
	now = now.UTC().Truncate(time.Second)
	ttl = ttl.Truncate(time.Second)
	amzDate := now.Format("20060102T150405Z")
	scopeDate := now.Format("20060102")
	credential := cfg.AccessKeyID + "/" + scopeDate + "/" + cfg.Region + "/s3/aws4_request"
	query := url.Values{}
	query.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	query.Set("X-Amz-Credential", credential)
	query.Set("X-Amz-Date", amzDate)
	query.Set("X-Amz-Expires", fmt.Sprintf("%d", int(ttl.Seconds())))
	escaped := escapeKey(key)
	canonicalURI := "/" + escaped
	if cfg.PathStyle {
		canonicalURI = "/" + url.PathEscape(cfg.Bucket) + "/" + escaped
	}
	host := endpoint.Host
	if !cfg.PathStyle {
		host = cfg.Bucket + "." + host
	}
	headers["host"] = host
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	signedHeaders := strings.Join(names, ";")
	canonicalHeaders := ""
	for _, name := range names {
		canonicalHeaders += name + ":" + headers[name] + "\n"
	}
	query.Set("X-Amz-SignedHeaders", signedHeaders)
	canonicalQuery := query.Encode()
	canonical := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		"UNSIGNED-PAYLOAD",
	}, "\n")
	scope := scopeDate + "/" + cfg.Region + "/s3/aws4_request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hexSHA256(canonical)
	signature := hex.EncodeToString(hmacSHA256(signingKey(cfg.SecretAccessKey, scopeDate, cfg.Region), stringToSign))
	query.Set("X-Amz-Signature", signature)
	signed := *endpoint
	signed.Host = host
	signed.Path = "/" + key
	if cfg.PathStyle {
		signed.Path = "/" + cfg.Bucket + "/" + key
	}
	signed.RawPath = canonicalURI
	signed.RawQuery = query.Encode()
	return Grant{
		URL:     signed.String(),
		Method:  method,
		Headers: headers,
		Expires: now.Add(ttl),
	}, nil
}

func validKey(key string) bool {
	return key != "" && !strings.HasPrefix(key, "/") && !strings.Contains(key, "..") && !strings.Contains(key, "\\") && !strings.Contains(key, " ")
}

func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func hexSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func signingKey(secret, date, region string) []byte {
	return hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+secret), date), region), "s3"), "aws4_request")
}
