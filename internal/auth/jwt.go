package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Claims contains the JWT claims used by the server.
type Claims struct {
	Sub       string `json:"sub"`
	Role      string `json:"role"`
	Namespace string `json:"namespace,omitempty"`
	Iat       int64  `json:"iat"`
	Exp       int64  `json:"exp"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

// ValidateJWT verifies the token signature and validates required claims.
func ValidateJWT(token, secret string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid JWT format")
	}

	var hdr header
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid header encoding: %w", err)
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return nil, fmt.Errorf("invalid header JSON: %w", err)
	}
	if hdr.Alg != "HS256" || hdr.Typ != "JWT" {
		return nil, fmt.Errorf("unsupported JWT header")
	}

	signingInput := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expectedSig), []byte(parts[2])) {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid payload encoding: %w", err)
	}

	var claims Claims
	if err := json.Unmarshal(payloadJSON, &claims); err != nil {
		return nil, fmt.Errorf("invalid payload JSON: %w", err)
	}
	if claims.Sub == "" {
		return nil, fmt.Errorf("missing sub claim")
	}
	if claims.Exp == 0 {
		return nil, fmt.Errorf("missing exp claim")
	}
	if time.Now().Unix() > claims.Exp {
		return nil, fmt.Errorf("token expired")
	}
	return &claims, nil
}

// GenerateJWT creates an HS256 JWT with the provided claims.
func GenerateJWT(claims Claims, secret string) (string, error) {
	hdr := header{Alg: "HS256", Typ: "JWT"}
	headerJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}

	headerPart := base64.RawURLEncoding.EncodeToString(headerJSON)
	payloadPart := base64.RawURLEncoding.EncodeToString(payloadJSON)
	signingInput := headerPart + "." + payloadPart

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput))
	sigPart := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	return signingInput + "." + sigPart, nil
}
