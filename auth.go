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

func getJWTSecret(secretFile string) (string, error) {
	data, err := os.ReadFile(secretFile)
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
	signature.Write([]byte(parts[0] + "." + parts[1]))
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
