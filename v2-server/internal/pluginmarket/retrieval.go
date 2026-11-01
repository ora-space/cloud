package pluginmarket

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Retrieval describes the non-secret part of the Desktop package download configuration.
type Retrieval struct {
	UseProxy bool   `json:"useProxy"`
	Mode     string `json:"mode"`
	Endpoint string `json:"endpoint,omitempty"`
	Bucket   string `json:"bucket,omitempty"`
	Region   string `json:"region,omitempty"`
}
type Credentials struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

var ErrCredentials = errors.New("market credential configuration unavailable")

func (r Retrieval) Validate() error {
	if r.UseProxy {
		if _, e := sourceProxy(); e != nil {
			return e
		}
	}
	switch r.Mode {
	case "", "direct_https":
		return nil
	case "s3_sigv4":
		u, e := url.Parse(r.Endpoint)
		if e != nil || u.Scheme != "https" || u.User != nil || u.Hostname() == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" || !regexp.MustCompile("^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$").MatchString(r.Bucket) || !regexp.MustCompile("^[A-Za-z0-9-]{1,63}$").MatchString(r.Region) {
			return ErrSource
		}
		return nil
	default:
		return ErrSource
	}
}
func sourceProxy() (string, error) {
	raw := os.Getenv("V2_MARKET_SOCKS_PROXY")
	u, e := url.Parse(raw)
	// socks5 (not socks5h) keeps DNS and the pinned IP under the client's control.
	if e != nil || u.Scheme != "socks5" || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrSource
	}
	return raw, nil
}
func marketCipher() (cipher.AEAD, error) {
	key, e := base64.StdEncoding.DecodeString(os.Getenv("V2_MARKET_CREDENTIAL_KEY"))
	if e != nil || len(key) != 32 {
		return nil, ErrCredentials
	}
	c, e := aes.NewCipher(key)
	if e != nil {
		return nil, ErrCredentials
	}
	return cipher.NewGCM(c)
}
func SealCredentials(c Credentials, binding string) ([]byte, error) {
	if strings.TrimSpace(c.AccessKey) == "" || strings.TrimSpace(c.SecretKey) == "" {
		return nil, ErrCredentials
	}
	a, e := marketCipher()
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(c)
	if e != nil {
		return nil, ErrCredentials
	}
	nonce := make([]byte, a.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return nil, ErrCredentials
	}
	return a.Seal(nonce, nonce, raw, []byte(binding)), nil
}
func OpenCredentials(ciphertext []byte, binding string) (Credentials, error) {
	a, e := marketCipher()
	if e != nil || len(ciphertext) < a.NonceSize() {
		return Credentials{}, ErrCredentials
	}
	raw, e := a.Open(nil, ciphertext[:a.NonceSize()], ciphertext[a.NonceSize():], []byte(binding))
	var c Credentials
	if e != nil || json.Unmarshal(raw, &c) != nil {
		return Credentials{}, ErrCredentials
	}
	return c, nil
}
