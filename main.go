package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "1.0.4"

func main() {
	preFlags := flag.NewFlagSet("pre", flag.ContinueOnError)
	preFlags.SetOutput(io.Discard)
	var configPath string
	var genConfig string
	preFlags.StringVar(&configPath, "config", "", "Path to JSON config file")
	preFlags.StringVar(&genConfig, "gen-config", "", "Generate config template file")
	_ = preFlags.Parse(os.Args[1:])

	if genConfig != "" {
		if err := saveConfigTemplate(genConfig); err != nil {
			log.Fatalf("Failed to generate config: %v", err)
		}
		fmt.Printf("Config template saved to %s\n", genConfig)
		os.Exit(0)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Config error: %v", err)
	}

	flag.StringVar(&configPath, "config", configPath, "Path to JSON config file")
	flag.StringVar(&genConfig, "gen-config", genConfig, "Generate config template file")
	flag.StringVar(&cfg.WSPort, "wsport", cfg.WSPort, "WebSocket port")
	flag.IntVar(&cfg.PCMPort, "pcmport", cfg.PCMPort, "UDP PCM input port")
	flag.StringVar(&cfg.UDPIP, "udpip", cfg.UDPIP, "UDP listen IP")
	flag.StringVar(&cfg.TLSCert, "cert", cfg.TLSCert, "TLS cert")
	flag.StringVar(&cfg.TLSKey, "key", cfg.TLSKey, "TLS key")
	flag.StringVar(&cfg.LogFile, "log", cfg.LogFile, "Log file")
	flag.StringVar(&cfg.JWTSecretPath, "jwtsecret", cfg.JWTSecretPath, "Path to the JWT secret file")
	flag.IntVar(&cfg.OpusBitrate, "bitrate", cfg.OpusBitrate, "Opus bitrate bps")
	flag.IntVar(&cfg.Channels, "channels", cfg.Channels, "Audio channels: 1 (mono) or 2 (stereo)")
	flag.StringVar(&cfg.Mode, "mode", cfg.Mode, "Encoder profile: speech or music")
	flag.IntVar(&cfg.MaxClients, "maxclients", cfg.MaxClients, "Max simultaneous WS clients (0 = unlimited)")
	flag.IntVar(&cfg.UDPWaitWarnSec, "udpwaitwarn", cfg.UDPWaitWarnSec, "Seconds to wait for first UDP audio before logging a warning (0 = disabled)")
	flag.IntVar(&cfg.StatsIntervalSec, "statsinterval", cfg.StatsIntervalSec, "Seconds between traffic-stats WS broadcasts (0 = disabled)")
	flag.BoolVar(&cfg.TestTone, "testtone", cfg.TestTone, "Generate test tone instead of UDP input")
	flag.BoolVar(&cfg.DebugJitter, "debugjitter", cfg.DebugJitter, "Log UDP gap diagnostics")
	flag.BoolVar(&cfg.NoTLS, "notls", cfg.NoTLS, "Plain WS (behind reverse proxy)")
	flag.BoolVar(&cfg.NoAuth, "noauth", cfg.NoAuth, "Skip JWT auth - default false. Set -noauth=true only for local testing without a token")
	showVersion := flag.Bool("v", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if cfg.Channels != 1 && cfg.Channels != 2 {
		log.Fatalf("-channels must be 1 (mono) or 2 (stereo), got %d", cfg.Channels)
	}
	if cfg.Mode != "speech" && cfg.Mode != "music" {
		log.Fatalf("-mode must be 'speech' or 'music', got %q", cfg.Mode)
	}

	logger := makeLogger(cfg.LogFile)
	logger.Println("OpusRelay (lightweight PCM-to-Opus streaming proxy)")
	logger.Printf("Version      : %s", version)
	logger.Printf("UDP Listen   : %s:%d", cfg.UDPIP, cfg.PCMPort)
	logger.Printf("WebSocket    : %s", cfg.WSPort)
	channelDesc := "mono"
	if cfg.Channels == 2 {
		channelDesc = "stereo"
	}
	logger.Printf("Opus bitrate : %d bps | frame %dms | %dHz %s | mode: %s",
		cfg.OpusBitrate, cfg.FrameMS, cfg.SampleRate, channelDesc, cfg.Mode)
	logger.Printf("Silence thr. : %dms", cfg.SilenceThresholdMS)

	if cfg.TestTone {
		logger.Println("TEST TONE: ON (UDP input disabled)")
	}
	if cfg.DebugJitter {
		logger.Println("UDP GAP DEBUG: ON")
	}

	if cfg.NoAuth {
		logger.Println("######################################################")
		logger.Println("# WARNING: AUTHENTICATION IS DISABLED (-noauth=true) #")
		logger.Println("######################################################")
	} else {
		secret, err := getJWTSecret(cfg.JWTSecretPath)
		if err != nil {
			logger.Printf("WARNING: JWT secret file not found: %v", err)
			logger.Printf("Please ensure %s exists and is readable", cfg.JWTSecretPath)
		}
		// Cached once here; every request reads cfg.JWTSecret from memory
		// instead of hitting the disk (see auth.go / server.go). If the
		// secret couldn't be read, cfg.JWTSecret stays "" and validateJWT
		// will correctly reject every token until the server is restarted
		// with a valid secret file.
		cfg.JWTSecret = secret
	}

	hub := NewHub()
	go pcmListener(*cfg, hub, logger)

	mux := http.NewServeMux()
	mux.Handle("/", securityMiddleware(http.HandlerFunc(wsHandler(*cfg, hub, logger))))

	srv := &http.Server{
		Addr:              ":" + cfg.WSPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second, // mitigates Slowloris-style slow-header attacks (gosec G112)
	}
	if !cfg.NoTLS {
		srv.TLSConfig = &tls.Config{
			MinVersion:       tls.VersionTLS12,
			CurvePreferences: []tls.CurveID{tls.X25519, tls.CurveP256},
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Printf("Received %v, shutting down...", sig)
		hub.CloseAll()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Printf("Shutdown error: %v", err)
		}
	}()

	var srvErr error
	if cfg.NoTLS {
		logger.Printf("Plain WS on ws://0.0.0.0:%s", cfg.WSPort)
		srvErr = srv.ListenAndServe()
	} else {
		logger.Printf("Secure WSS on wss://0.0.0.0:%s", cfg.WSPort)
		srvErr = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	}
	if srvErr != nil && srvErr != http.ErrServerClosed {
		logger.Fatalf("Server: %v", srvErr)
	}
	logger.Println("Server stopped")
}
