package pluginmarket

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var ErrSource = errors.New("invalid public HTTPS marketplace source")

func ValidatePublicSource(raw, branch string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Port() != "" && u.Port() != "443" || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || !ValidBranch(branch) {
		return nil, ErrSource
	}
	host := strings.ToLower(u.Hostname())
	if strings.ContainsAny(host, "%,+:[]") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return nil, ErrSource
	}
	if ip, e := netip.ParseAddr(host); e == nil && !publicIP(ip) {
		return nil, ErrSource
	}
	return u, nil
}

// Namespace is the identity Desktop freezes at source creation, even if the URL is edited.
func Namespace(u *url.URL) string {
	canonical := "https://" + strings.ToLower(u.Hostname()) + strings.TrimSuffix(strings.TrimRight(strings.ToLower(u.EscapedPath()), "/"), ".git")
	digest := sha256.Sum256([]byte(canonical))
	slug := strings.Trim(regexp.MustCompile("[^a-z0-9]+").ReplaceAllString(path.Base(strings.TrimPrefix(canonical, "https://"+strings.ToLower(u.Hostname()))), "-"), "-")
	if slug == "" {
		slug = "marketplace"
	}
	if len(slug) > 24 {
		slug = slug[:24]
	}
	return slug + "." + hex.EncodeToString(digest[:4])
}

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2001:db8::/32", "2002::/16"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return ip.Is4() || netip.MustParsePrefix("2000::/3").Contains(ip)
}

// Pin the resolved address for the entire clone. Never inherit host git configuration,
// credential helpers or proxy settings for a repository chosen by a space owner.
func ReadPublic(ctx context.Context, source Source, useProxy bool) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	u, e := ValidatePublicSource(source.URL, source.Branch)
	if e != nil {
		return nil, e
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if e != nil || len(ips) == 0 {
		return nil, ErrSource
	}
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, ErrSource
		}
	}
	address := net.JoinHostPort(ips[0].Unmap().String(), "443")
	tmp, e := os.MkdirTemp("", "ora-market-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(tmp)
	dst := filepath.Join(tmp, "repo")
	proxy := ""
	if useProxy {
		proxy, e = sourceProxy()
		if e != nil {
			return nil, e
		}
	}
	tunnel, closeTunnel, e := gitTunnel(ctx, u.Hostname(), address, proxy)
	if e != nil {
		return nil, e
	}
	defer closeTunnel()
	cmd := exec.CommandContext(ctx, "git", "-c", "http.followRedirects=false", "-c", "http.proxy="+tunnel, "-c", "protocol.allow=never", "-c", "protocol.https.allow=always", "clone", "--depth=1", "--single-branch", "--no-tags", "--branch", source.Branch, "--", source.URL, dst)
	cmd.Dir = tmp
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_TERMINAL_PROMPT=0"}
	if e := cmd.Run(); e != nil {
		return nil, errors.New("marketplace checkout failed")
	}
	entries, skipped, e := Scan(filepath.Join(dst, "registry"), source.Namespace, source.URL)
	if e != nil || len(skipped) > 0 {
		return nil, errors.New("invalid marketplace snapshot")
	}
	if len(entries) == 0 {
		return nil, errors.New("marketplace has no visible non-Agent entries")
	}
	return entries, nil
}
