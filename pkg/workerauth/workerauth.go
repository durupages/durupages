// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

// Package workerauth implements the short-lived worker JWT that the controller
// issues to each worker pod and the hub verifies offline. The controller signs
// with an Ed25519 private key; the hub (and any other verifier) checks the
// signature with the matching public key, so no online lookup to the controller
// is required.
//
// The token carries the pod name (sub), the tenant it is scoped to (the
// "tenant" claim), plus the standard iat/exp/jti registered claims. Verifiers
// enforce the EdDSA algorithm and a present expiry to close the usual JWT
// pitfalls (alg confusion, never-expiring tokens).
package workerauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// alg is the only JWT signing algorithm accepted by Issue and Verify.
const alg = "EdDSA"

// Claims is the decoded, caller-facing representation of a worker token.
type Claims struct {
	// Pod is the worker pod name, carried in the JWT "sub" claim.
	Pod string
	// Tenant is the tenant the pod is scoped to, carried in the "tenant" claim.
	Tenant string
}

// registeredClaims is the on-the-wire claim set: the standard registered claims
// plus the custom tenant claim. The pod name rides in RegisteredClaims.Subject.
type registeredClaims struct {
	Tenant string `json:"tenant"`
	jwt.RegisteredClaims
}

// Issue signs a worker token for pod/tenant valid for ttl. Each token gets a
// random jti so otherwise-identical tokens remain distinguishable.
func Issue(priv ed25519.PrivateKey, pod, tenant string, ttl time.Duration) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("workerauth: invalid ed25519 private key")
	}
	jti, err := randomID()
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := registeredClaims{
		Tenant: tenant,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   pod,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return tok.SignedString(priv)
}

// Verify checks token against pub, enforcing the EdDSA algorithm and a present
// expiry, and returns the decoded claims. Tokens signed with any other
// algorithm (e.g. an HS256 alg-confusion attempt), expired tokens and tampered
// tokens are rejected.
func Verify(pub ed25519.PublicKey, token string) (*Claims, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("workerauth: invalid ed25519 public key")
	}
	var claims registeredClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("workerauth: unexpected signing method %q", t.Method.Alg())
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{alg}), jwt.WithExpirationRequired())
	if err != nil {
		return nil, err
	}
	return &Claims{Pod: claims.Subject, Tenant: claims.Tenant}, nil
}

// GenerateKey returns a fresh Ed25519 key pair suitable for Issue/Verify.
func GenerateKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// randomID returns a URL-safe random identifier for use as a jti.
func randomID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// MarshalPrivateKeyPEM encodes an Ed25519 private key as PKCS#8 PEM.
func MarshalPrivateKeyPEM(priv ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKeyPEM decodes a PKCS#8 PEM-encoded Ed25519 private key.
func ParsePrivateKeyPEM(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("workerauth: no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("workerauth: not an ed25519 private key (%T)", key)
	}
	return priv, nil
}

// MarshalPublicKeyPEM encodes an Ed25519 public key as PKIX PEM.
func MarshalPublicKeyPEM(pub ed25519.PublicKey) ([]byte, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// ParsePublicKeyPEM decodes a PKIX PEM-encoded Ed25519 public key.
func ParsePublicKeyPEM(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("workerauth: no PEM block found")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("workerauth: not an ed25519 public key (%T)", key)
	}
	return pub, nil
}
