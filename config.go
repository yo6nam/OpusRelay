package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
)

func defaultLogPath(goos string) string {
	if goos == "windows" {
		return filepath.Join(os.TempDir(), "opus_relay.log")
	}
	return "/var/log/opus_relay.log"
}

func defaultJWTSecretPath(goos string) string {
	if goos == "windows" {
		return filepath.Join(os.TempDir(), "jwt.secret")
	}
	return "/opt/jwt.secret"
}

type Config struct {
	WSPort             string `json:"ws_port"`
	PCMPort            int    `json:"pcm_port"`
	UDPIP              string `json:"udp_ip"`
	TLSCert            string `json:"tls_cert"`
	TLSKey             string `json:"tls_key"`
	LogFile            string `json:"log_file"`
	JWTSecretPath      string `json:"jwt_secret_path"`
	OpusBitrate        int    `json:"opus_bitrate"`
	SampleRate         int    `json:"sample_rate"`
	Channels           int    `json:"channels"`
	Mode               string `json:"mode"`
	FrameMS            int    `json:"frame_ms"`
	TestTone           bool   `json:"test_tone"`
	DebugJitter        bool   `json:"debug_jitter"`
	NoTLS              bool   `json:"no_tls"`
	SilenceThresholdMS int    `json:"silence_threshold_ms"`
	MaxClients         int    `json:"max_clients"`
	NoAuth             bool   `json:"no_auth"`
	UDPWaitWarnSec     int    `json:"udp_wait_warn_sec"`
	StatsIntervalSec   int    `json:"stats_interval_sec"`
}

func loadConfig(configPath string) (*Config, error) {
	cfg := &Config{
		WSPort:             "8080",
		PCMPort:            1235,
		UDPIP:              "127.0.0.1",
		TLSCert:            "",
		TLSKey:             "",
		LogFile:            defaultLogPath(runtime.GOOS),
		JWTSecretPath:      defaultJWTSecretPath(runtime.GOOS),
		OpusBitrate:        16000,
		SampleRate:         48000,
		Channels:           1,
		Mode:               "speech",
		FrameMS:            20,
		TestTone:           false,
		DebugJitter:        false,
		NoTLS:              false,
		SilenceThresholdMS: 300,
		MaxClients:         500,
		NoAuth:             false,
		UDPWaitWarnSec:     10,
		StatsIntervalSec:   2,
	}

	if configPath != "" {
		file, err := os.Open(configPath)
		if err == nil {
			defer file.Close()
			decoder := json.NewDecoder(file)
			if err := decoder.Decode(cfg); err != nil {
				return nil, fmt.Errorf("error parsing config file: %v", err)
			}
			log.Printf("Loaded configuration from %s", configPath)
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("error opening config file: %v", err)
		}
	}

	return cfg, nil
}

func saveConfigTemplate(path string) error {
	template := Config{
		WSPort:             "8080",
		PCMPort:            1235,
		UDPIP:              "127.0.0.1",
		TLSCert:            "/path/to/cert.pem",
		TLSKey:             "/path/to/key.pem",
		LogFile:            defaultLogPath(runtime.GOOS),
		JWTSecretPath:      defaultJWTSecretPath(runtime.GOOS),
		OpusBitrate:        16000,
		SampleRate:         48000,
		Channels:           1,
		Mode:               "speech",
		FrameMS:            20,
		TestTone:           false,
		DebugJitter:        false,
		NoTLS:              false,
		SilenceThresholdMS: 300,
		MaxClients:         500,
		NoAuth:             false,
		UDPWaitWarnSec:     10,
		StatsIntervalSec:   2,
	}

	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(template)
}
