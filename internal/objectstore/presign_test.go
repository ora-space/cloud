package objectstore

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestPresignPUTBindsTheKeyAndStaysLocal(t *testing.T) {
	cfg := Config{
		Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "revisions", PathStyle: true,
		AccessKeyID: "test-access", SecretAccessKey: "test-secret", UploadGrantTTL: 15 * time.Minute,
	}
	now := time.Date(2026, 9, 30, 1, 2, 3, 0, time.UTC)
	grant, err := PresignPUT(&cfg, "revisions/tenant/run/work/revision.bundle", now)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Method != "PUT" || !grant.Expires.Equal(now.Add(15*time.Minute)) {
		t.Fatal("incorrect grant method or expiry")
	}
	if !strings.Contains(grant.URL, "/revisions/revisions/tenant/run/work/revision.bundle") || !strings.Contains(grant.URL, "X-Amz-Signature=") {
		t.Fatal("grant did not bind the canonical key and signature")
	}
	if grant.Headers["host"] != "127.0.0.1:9000" || grant.Headers["if-none-match"] != "*" {
		t.Fatalf("headers = %v", grant.Headers)
	}
	other, err := PresignPUT(&cfg, "revisions/tenant/run/work/session.jsonl", now)
	if err != nil {
		t.Fatal(err)
	}
	if other.URL == grant.URL {
		t.Fatal("different keys produced the same url")
	}
	if _, err = PresignPUT(&Config{}, "revisions/a", now); err == nil {
		t.Fatal("unconfigured store signed a url")
	}
	if _, err = PresignPUT(&cfg, "../escape", now); err == nil {
		t.Fatal("escaped key was signed")
	}
}

func TestPresignPUTChecksumBindsHeaderAndRejectsInvalidDigest(t *testing.T) {
	cfg := Config{Endpoint: "http://127.0.0.1:9000", Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret"}
	grant, err := PresignPUTChecksum(&cfg, "run/history", strings.Repeat("0", 64), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal("invalid generated grant")
	}
	if grant.Headers["x-amz-checksum-sha256"] != base64.StdEncoding.EncodeToString(make([]byte, 32)) || !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "x-amz-checksum-sha256") {
		t.Fatal("checksum was not bound into the signature")
	}
	if grant.Headers["if-none-match"] != "*" || !strings.Contains(u.Query().Get("X-Amz-SignedHeaders"), "if-none-match") {
		t.Fatal("create-only condition was not bound into the signature")
	}
	for _, digest := range []string{"", "invalid", strings.Repeat("A", 64)} {
		if _, err := PresignPUTChecksum(&cfg, "run/history", digest, time.Now()); err == nil {
			t.Fatal("invalid digest was authorized")
		}
	}
}

func TestPresignPublicEndpointAndExpiryMatchSignedPrecision(t *testing.T) {
	cfg := Config{Endpoint: "http://private.invalid:9000", PublicEndpoint: "https://public.invalid", Region: "us-east-1", Bucket: "revisions", PathStyle: true, AccessKeyID: "test-access", SecretAccessKey: "test-secret", UploadGrantTTL: 1500 * time.Millisecond}
	now := time.Date(2026, 9, 30, 1, 2, 3, 500000000, time.UTC)
	grant, err := PresignPUT(&cfg, "revisions/work/session.jsonl", now)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(grant.URL)
	if err != nil {
		t.Fatal("invalid generated URL")
	}
	if u.Host != "public.invalid" || grant.Headers["host"] != u.Host || u.Query().Get("X-Amz-Expires") != "1" || !grant.Expires.Equal(now.Truncate(time.Second).Add(time.Second)) {
		t.Fatal("public endpoint or signed expiration differed from the grant")
	}
}

func TestPresignPUTUsesTheCanonicalObjectPath(t *testing.T) {
	for _, pathStyle := range []bool{true, false} {
		cfg := Config{Endpoint: "https://objects.example.invalid", Region: "us-east-1", Bucket: "revisions", PathStyle: pathStyle, AccessKeyID: "test-access", SecretAccessKey: "test-secret"}
		key := "runs/中文/100%.bundle"
		grant, err := PresignPUT(&cfg, key, time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatal(err)
		}
		signed, err := url.Parse(grant.URL)
		if err != nil {
			t.Fatal(err)
		}
		wantPath, wantHost := "/"+key, "revisions.objects.example.invalid"
		if pathStyle {
			wantPath, wantHost = "/revisions/"+key, "objects.example.invalid"
		}
		if signed.Path != wantPath || signed.Host != wantHost || signed.EscapedPath() != escapeKey(wantPath) {
			t.Fatalf("path style %v: host=%s path=%s raw=%s", pathStyle, signed.Host, signed.Path, signed.EscapedPath())
		}
	}
}
