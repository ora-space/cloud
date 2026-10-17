package cloneauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// LoadPrivateKey reads one Ed25519 private key from a PEM file holding exactly one PKCS#8
// "PRIVATE KEY" block (what `openssl genpkey -algorithm ED25519` writes).
//
// Keys arrive as mounted files, never as configuration values. A second block is refused rather than
// resolved by position, because which key signs must not be ambiguous.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	blocks, err := readBlocks(path, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	if len(blocks) != 1 {
		return nil, fmt.Errorf("%w: a private key file must hold exactly one PEM block", ErrKey)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(blocks[0].Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: not a PKCS#8 private key", ErrKey)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%w: not an Ed25519 private key", ErrKey)
	}
	return key, nil
}

// LoadPublicKeys reads every Ed25519 public key from a PEM file of one or more PKIX "PUBLIC KEY"
// blocks.
//
// Several blocks are how a deployment rotates: add the incoming key, switch the signer, wait out the
// maximum lifetime, remove the outgoing key. Every block must be usable; a file mixing a good key with
// a bad one is refused, because silently verifying against a subset would hide that the key an
// operator just added is not in force.
func LoadPublicKeys(path string) ([]ed25519.PublicKey, error) {
	blocks, err := readBlocks(path, "PUBLIC KEY")
	if err != nil {
		return nil, err
	}
	keys := make([]ed25519.PublicKey, 0, len(blocks))
	for _, block := range blocks {
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("%w: not a PKIX public key", ErrKey)
		}
		key, ok := parsed.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%w: not an Ed25519 public key", ErrKey)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// readBlocks reads every PEM block of one file and requires each to have blockType. Errors never echo
// the file's content, so a misconfigured path cannot print key material.
func readBlocks(path, blockType string) ([]*pem.Block, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: no key file is configured", ErrKey)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the path is deployment configuration, not request input.
	if err != nil {
		return nil, fmt.Errorf("%w: read key file: %w", ErrKey, err)
	}
	var blocks []*pem.Block
	for rest := raw; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != blockType {
			return nil, fmt.Errorf("%w: PEM block %q is not %q", ErrKey, block.Type, blockType)
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%w: the key file holds no PEM block", ErrKey)
	}
	return blocks, nil
}
