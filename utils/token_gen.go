// gen-test-token creates a JWT compatible with webproxy's validateJWT(),
// for local testing only. Do NOT use this to issue real user tokens in
// production — your actual auth backend should do that.
//
// Usage:
//
//	go run utils/token_gen.go -email test@example.com -level admin -ttl 1h
//	go run utils/token_gen.go -secret-file /opt/jwt.secret -ttl 10m
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

type payload struct {
	Email string `json:"email"`
	Level string `json:"level"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

func b64(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func main() {
	var secretFile, email, level string
	var ttl time.Duration

	flag.StringVar(&secretFile, "secret-file", "/opt/jwt.secret", "Path to the JWT secret file (must match the server's)")
	flag.StringVar(&email, "email", "test@example.com", "Email to embed in the token")
	flag.StringVar(&level, "level", "listener", "Level/role to embed in the token")
	flag.DurationVar(&ttl, "ttl", time.Hour, "Token validity, e.g. 1h, 30m")
	flag.Parse()

	secretBytes, err := os.ReadFile(secretFile) // #nosec G304 -- secretFile comes from the -secret-file CLI flag of this local test-only tool, not remote input
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read secret file %s: %v\n", secretFile, err)
		fmt.Fprintf(os.Stderr, "hint: for a local test you can create one with:\n  openssl rand -hex 32 > %s\n", secretFile)
		os.Exit(1)
	}
	secret := strings.TrimSpace(string(secretBytes))

	now := time.Now()
	h := header{Alg: "HS256", Typ: "JWT"}
	p := payload{
		Email: email,
		Level: level,
		Iat:   now.Unix(),
		Exp:   now.Add(ttl).Unix(),
	}

	hBytes, _ := json.Marshal(h)
	pBytes, _ := json.Marshal(p)

	signingInput := b64(hBytes) + "." + b64(pBytes)

	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(signingInput)) // hash.Hash.Write never returns a non-nil error, per the io.Writer contract in crypto/*
	sig := b64(mac.Sum(nil))

	token := signingInput + "." + sig

	fmt.Println(token)
	fmt.Fprintf(os.Stderr, "\nemail=%s level=%s expiră la %s (peste %s)\n",
		email, level, time.Unix(p.Exp, 0).Format(time.RFC3339), ttl)
	fmt.Fprintf(os.Stderr, "conectare test: wss://<host>:<wsport>/?token=%s\n", token)
}
