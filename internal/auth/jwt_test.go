package auth

import (
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func TestValidateJWTValid(t *testing.T) {
	token, err := GenerateJWT(Claims{
		Sub:  "user@example.com",
		Role: "operator",
		Iat:  time.Now().Add(-time.Minute).Unix(),
		Exp:  time.Now().Add(time.Hour).Unix(),
	}, testSecret)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}

	claims, err := ValidateJWT(token, testSecret)
	if err != nil {
		t.Fatalf("ValidateJWT: %v", err)
	}
	if claims.Sub != "user@example.com" || claims.Role != "operator" {
		t.Fatalf("unexpected claims: %#v", claims)
	}
}

func TestValidateJWTExpired(t *testing.T) {
	token, err := GenerateJWT(Claims{
		Sub: "user@example.com",
		Exp: time.Now().Add(-time.Hour).Unix(),
	}, testSecret)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	_, err = ValidateJWT(token, testSecret)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestValidateJWTInvalidSignature(t *testing.T) {
	token, err := GenerateJWT(Claims{
		Sub: "user@example.com",
		Exp: time.Now().Add(time.Hour).Unix(),
	}, testSecret)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	token = token[:len(token)-1] + "x"
	_, err = ValidateJWT(token, testSecret)
	if err == nil || !strings.Contains(err.Error(), "invalid signature") {
		t.Fatalf("expected invalid signature error, got %v", err)
	}
}

func TestValidateJWTMissingSub(t *testing.T) {
	token, err := GenerateJWT(Claims{
		Exp: time.Now().Add(time.Hour).Unix(),
	}, testSecret)
	if err != nil {
		t.Fatalf("GenerateJWT: %v", err)
	}
	_, err = ValidateJWT(token, testSecret)
	if err == nil || !strings.Contains(err.Error(), "missing sub") {
		t.Fatalf("expected missing sub error, got %v", err)
	}
}
