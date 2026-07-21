// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerauth

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// LeaseClaims authenticate one proxied request towards the worker shim.
// The controller issues a lease token per granted slot; the router forwards
// it in the X-DuruPages-Lease header and the shim verifies it before letting
// the request reach the runtime.
type LeaseClaims struct {
	LeaseID      string
	TenantID     string
	PageID       string
	DeploymentID string
	RequestID    string
}

type leaseRegisteredClaims struct {
	jwt.RegisteredClaims
	Tenant     string `json:"tenant"`
	Page       string `json:"page"`
	Deployment string `json:"dep"`
	Request    string `json:"req"`
}

// IssueLease signs lease claims with the controller's ed25519 key. The ttl
// should cover the request deadline (page RequestTimeout plus slack).
func IssueLease(priv ed25519.PrivateKey, c LeaseClaims, ttl time.Duration) (string, error) {
	if c.LeaseID == "" || c.TenantID == "" || c.PageID == "" {
		return "", errors.New("workerauth: lease claims incomplete")
	}
	now := time.Now()
	claims := leaseRegisteredClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   c.LeaseID,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
		Tenant:     c.TenantID,
		Page:       c.PageID,
		Deployment: c.DeploymentID,
		Request:    c.RequestID,
	}
	return jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(priv)
}

// VerifyLease validates a lease token and returns its claims.
func VerifyLease(pub ed25519.PublicKey, token string) (*LeaseClaims, error) {
	var claims leaseRegisteredClaims
	parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != alg {
			return nil, fmt.Errorf("workerauth: unexpected alg %q", t.Method.Alg())
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{alg}), jwt.WithExpirationRequired())
	if err != nil {
		return nil, fmt.Errorf("workerauth: verify lease: %w", err)
	}
	if !parsed.Valid || claims.Subject == "" || claims.Tenant == "" || claims.Page == "" {
		return nil, errors.New("workerauth: invalid lease token")
	}
	return &LeaseClaims{
		LeaseID:      claims.Subject,
		TenantID:     claims.Tenant,
		PageID:       claims.Page,
		DeploymentID: claims.Deployment,
		RequestID:    claims.Request,
	}, nil
}
