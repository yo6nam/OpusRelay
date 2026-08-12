package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type JWTPayload struct {
	Email string `json:"email"`
	Level string `json:"level"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

// getJWTSecret reads the JWT secret from disk. secretFile always comes from
// -jwtsecret / config.go's JWTSecretPath default — an operator-supplied CLI
// flag or config file, not remote/user input — so this isn't attacker
// -controlled path traversal (gosec G304). Called once at startup only; see
// main.go and Config.JWTSecret for why the server no longer re-reads this
// file on every request.
func getJWTSecret(secretFile string) (string, error) {
	data, err := os.ReadFile(secretFile) // #nosec G304 -- operator-supplied path, see doc comment above
	if err != nil {
		return "", fmt.Errorf("cannot read JWT secret from %s: %v", secretFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func validateJWT(tokenString string, secret string) (*JWTPayload, error) {
	if tokenString == "" {
		return nil, fmt.Errorf("empty token")
	}
	if secret == "" {
		return nil, fmt.Errorf("no JWT secret loaded")
	}

	parts := strings.Split(tokenString, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid token format")
	}

	signature := hmac.New(sha256.New, []byte(secret))
	_, _ = signature.Write([]byte(parts[0] + "." + parts[1])) // hash.Hash.Write never returns a non-nil error, per the io.Writer contract in crypto/*
	expectedSignature := base64.RawURLEncoding.EncodeToString(signature.Sum(nil))

	if !hmac.Equal([]byte(parts[2]), []byte(expectedSignature)) {
		return nil, fmt.Errorf("invalid signature")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid payload: %v", err)
	}

	var payload JWTPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, fmt.Errorf("cannot parse payload: %v", err)
	}

	if payload.Exp < time.Now().Unix() {
		return nil, fmt.Errorf("token expired")
	}

	return &payload, nil
}
