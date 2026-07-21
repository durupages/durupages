// Copyright 2026 JC-Lab
// SPDX-License-Identifier: EPL-2.0

package workerauth

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestIssueVerifyRoundtrip(t *testing.T) {
	pub, priv, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Issue(priv, "pod-1", "tenant-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Verify(pub, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Pod != "pod-1" {
		t.Errorf("pod = %q, want pod-1", claims.Pod)
	}
	if claims.Tenant != "tenant-a" {
		t.Errorf("tenant = %q, want tenant-a", claims.Tenant)
	}
}

func TestVerifyWrongKey(t *testing.T) {
	_, priv, _ := GenerateKey()
	otherPub, _, _ := GenerateKey()
	tok, err := Issue(priv, "pod-1", "tenant-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(otherPub, tok); err == nil {
		t.Fatal("expected verification against wrong key to fail")
	}
}

func TestVerifyExpired(t *testing.T) {
	pub, priv, _ := GenerateKey()
	tok, err := Issue(priv, "pod-1", "tenant-a", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(pub, tok); err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

// TestVerifyAlgConfusion ensures a token signed with HS256 (using the public
// key bytes as the HMAC secret, the classic alg-confusion attack) is rejected.
func TestVerifyAlgConfusion(t *testing.T) {
	pub, _, _ := GenerateKey()
	forged := jwt.NewWithClaims(jwt.SigningMethodHS256, registeredClaims{
		Tenant: "tenant-a",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "pod-1",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	// Sign with the public key bytes treated as an HMAC secret.
	tok, err := forged.SignedString([]byte(pub))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(pub, tok); err == nil {
		t.Fatal("expected HS256 alg-confusion token to be rejected")
	}
}

func TestVerifyTamperedPayload(t *testing.T) {
	pub, priv, _ := GenerateKey()
	tok, err := Issue(priv, "pod-1", "tenant-a", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a character in the payload segment.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	b := []byte(parts[1])
	if b[0] == 'a' {
		b[0] = 'b'
	} else {
		b[0] = 'a'
	}
	parts[1] = string(b)
	if _, err := Verify(pub, strings.Join(parts, ".")); err == nil {
		t.Fatal("expected tampered token to be rejected")
	}
}

func TestVerifyMissingExpiry(t *testing.T) {
	pub, priv, _ := GenerateKey()
	// Craft a token with no exp claim.
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, registeredClaims{
		Tenant:           "tenant-a",
		RegisteredClaims: jwt.RegisteredClaims{Subject: "pod-1"},
	})
	signed, err := tok.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(pub, signed); err == nil {
		t.Fatal("expected token without expiry to be rejected")
	}
}

func TestPrivateKeyPEMRoundtrip(t *testing.T) {
	_, priv, _ := GenerateKey()
	data, err := MarshalPrivateKeyPEM(priv)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePrivateKeyPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	if !priv.Equal(got) {
		t.Fatal("private key did not survive PEM roundtrip")
	}
}

func TestPublicKeyPEMRoundtrip(t *testing.T) {
	pub, _, _ := GenerateKey()
	data, err := MarshalPublicKeyPEM(pub)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParsePublicKeyPEM(data)
	if err != nil {
		t.Fatal(err)
	}
	if !pub.Equal(got) {
		t.Fatal("public key did not survive PEM roundtrip")
	}
}

func TestPEMKeysInteroperate(t *testing.T) {
	pub, priv, _ := GenerateKey()
	privPEM, _ := MarshalPrivateKeyPEM(priv)
	pubPEM, _ := MarshalPublicKeyPEM(pub)
	priv2, err := ParsePrivateKeyPEM(privPEM)
	if err != nil {
		t.Fatal(err)
	}
	pub2, err := ParsePublicKeyPEM(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Issue(priv2, "pod-x", "tenant-y", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Verify(pub2, tok)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Pod != "pod-x" || claims.Tenant != "tenant-y" {
		t.Fatalf("unexpected claims: %+v", claims)
	}
}

func TestParseInvalidPEM(t *testing.T) {
	if _, err := ParsePrivateKeyPEM([]byte("not pem")); err == nil {
		t.Error("expected error for invalid private PEM")
	}
	if _, err := ParsePublicKeyPEM([]byte("not pem")); err == nil {
		t.Error("expected error for invalid public PEM")
	}
}
