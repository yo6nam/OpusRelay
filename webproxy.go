package main

/*
#cgo CFLAGS: -I/usr/include/opus
#cgo LDFLAGS: -lopus

#include <opus/opus.h>
#include <stdlib.h>
#include <math.h>

typedef struct { OpusEncoder *enc; } GoOpusEncoder;

GoOpusEncoder* opus_encoder_create_wrapper(int sampleRate, int channels, int bitrate, int musicMode, int *err) {
    GoOpusEncoder *e = (GoOpusEncoder*)malloc(sizeof(GoOpusEncoder));
    int application = musicMode ? OPUS_APPLICATION_AUDIO : OPUS_APPLICATION_VOIP;
    e->enc = opus_encoder_create(sampleRate, channels, application, err);
    if (*err != OPUS_OK || e->enc == NULL) { free(e); return NULL; }

    opus_encoder_ctl(e->enc, OPUS_SET_BITRATE(bitrate));
    if (musicMode) {
        opus_encoder_ctl(e->enc, OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC));
        opus_encoder_ctl(e->enc, OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_FULLBAND));
        opus_encoder_ctl(e->enc, OPUS_SET_COMPLEXITY(10));
    } else {
        opus_encoder_ctl(e->enc, OPUS_SET_SIGNAL(OPUS_SIGNAL_VOICE));
        opus_encoder_ctl(e->enc, OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_WIDEBAND));
        opus_encoder_ctl(e->enc, OPUS_SET_COMPLEXITY(7));
    }
    opus_encoder_ctl(e->enc, OPUS_SET_VBR(1));
    opus_encoder_ctl(e->enc, OPUS_SET_VBR_CONSTRAINT(1));
    opus_encoder_ctl(e->enc, OPUS_SET_INBAND_FEC(1));
    opus_encoder_ctl(e->enc, OPUS_SET_PACKET_LOSS_PERC(5));
    opus_encoder_ctl(e->enc, OPUS_SET_LSB_DEPTH(16));

    return e;
}

int opus_encode_wrapper(GoOpusEncoder *e, const opus_int16 *pcm, int frameSamples,
                        unsigned char *out, int maxOut) {
    return opus_encode(e->enc, pcm, frameSamples, out, maxOut);
}

void opus_encoder_destroy_wrapper(GoOpusEncoder *e) {
    if (e) { opus_encoder_destroy(e->enc); free(e); }
}
*/
import "C"

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const version = "1.0.1"
const maxAccumulatorBytes = 48000 * 2 * 4
const clientQueueDepth = 200

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

type JWTPayload struct {
	Email string `json:"email"`
	Level string `json:"level"`
	Iat   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
}

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
const controlQueueDepth = 20

type wsMessage struct {
	data   []byte
	isText bool
}

type wsConn struct {
	conn         net.Conn
	mu           sync.Mutex
	closed       bool
	sendQueue    chan wsMessage // binary audio frames
	controlQueue chan wsMessage // text control frames — always drained first
	done         chan struct{}
	logger       *log.Logger
	pingSentAt   atomic.Int64 // unix nano; 0 means no ping outstanding
}

type Hub struct {
	mu      sync.RWMutex
	clients map[*wsConn]struct{}
}

type opusEncoder struct {
	ptr *C.GoOpusEncoder
	mu  sync.Mutex
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

func getJWTSecret(secretFile string) (string, error) {
	data, err := os.ReadFile(secretFile)
	if err != nil {
		return "", fmt.Errorf("cannot read JWT secret from %s: %v", secretFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func validateJWT(tokenString string, secretPath string) (*JWTPayload, error) {
	if tokenString == "" {
		return nil, fmt.Errorf("empty token")
	}

	secret, err := getJWTSecret(secretPath)
	if err != nil {
		return nil, err
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

func NewHub() *Hub {
	return &Hub{clients: make(map[*wsConn]struct{})}
}

func (h *Hub) Add(c *wsConn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) Remove(c *wsConn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *Hub) Count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

func (h *Hub) CloseAll() {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.mu.Lock()
		if !c.closed {
			closeFrame := []byte{0x88, 0x02, 0x03, 0xE8} // 1000 Normal Closure
			c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
			c.conn.Write(closeFrame)
			c.closed = true
			c.conn.Close()
		}
		c.mu.Unlock()
	}
}

func (h *Hub) Broadcast(data []byte) {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		pkt := make([]byte, len(data))
		copy(pkt, data)
		c.enqueue(pkt)
	}
}

func (h *Hub) BroadcastControl(msg string) {
	h.mu.RLock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.RUnlock()

	for _, c := range clients {
		c.enqueueControl(msg)
	}
}

func upgradeWS(w http.ResponseWriter, r *http.Request, logger *log.Logger) (*wsConn, error) {
	if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
		return nil, fmt.Errorf("not a websocket request")
	}
	key := r.Header.Get("Sec-Websocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}

	h := sha1.New()
	io.WriteString(h, key+wsGUID)
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("hijacking not supported")
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	buf.WriteString(resp)
	if err := buf.Flush(); err != nil {
		conn.Close()
		return nil, err
	}

	c := &wsConn{
		conn:         conn,
		sendQueue:    make(chan wsMessage, clientQueueDepth),
		controlQueue: make(chan wsMessage, controlQueueDepth),
		done:         make(chan struct{}),
		logger:       logger,
	}
	go c.writeLoop()
	go c.readLoop()
	return c, nil
}

func (c *wsConn) writeLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {

		select {
		case msg, ok := <-c.controlQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
			continue
		default:
		}

		select {
		case msg, ok := <-c.controlQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
		case msg, ok := <-c.sendQueue:
			if !ok {
				return
			}
			if err := c.writeFrame(msg.data, msg.isText); err != nil {
				return
			}
		case <-ticker.C:
			c.pingSentAt.Store(time.Now().UnixNano())
			if err := c.writeFrame(nil, false, 0x9); err != nil {
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *wsConn) writeFrame(data []byte, isText bool, opcode ...byte) error {
	var header [10]byte
	switch {
	case len(opcode) > 0:
		header[0] = 0x80 | opcode[0] // FIN + explicit opcode (e.g. 0x9 = ping)
	case isText:
		header[0] = 0x81
	default:
		header[0] = 0x82
	}
	n := len(data)
	var hLen int
	switch {
	case n <= 125:
		header[1] = byte(n)
		hLen = 2
	case n <= 65535:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:], uint16(n))
		hLen = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:], uint64(n))
		hLen = 10
	}
	frame := make([]byte, hLen+n)
	copy(frame, header[:hLen])
	copy(frame[hLen:], data)
	c.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := c.conn.Write(frame)
	return err
}

func (c *wsConn) enqueue(data []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	msg := wsMessage{data: data, isText: false}
	select {
	case c.sendQueue <- msg:
		return true
	default:
		select {
		case <-c.sendQueue:
		default:
		}
		c.sendQueue <- msg
		return true
	}
}

func (c *wsConn) enqueueControl(msg string) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	c.mu.Unlock()

	m := wsMessage{data: []byte(msg), isText: true}
	select {
	case c.controlQueue <- m:
		return true
	default:
		select {
		case <-c.controlQueue:
		default:
		}
		c.controlQueue <- m
		return true
	}
}

func (c *wsConn) readLoop() {
	defer func() {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			c.conn.Close()
		}
		c.mu.Unlock()
		close(c.done)
		close(c.sendQueue)
		close(c.controlQueue)
	}()

	const idleTimeout = 90 * time.Second
	buf := bufio.NewReaderSize(c.conn, 4096)
	for {
		c.conn.SetReadDeadline(time.Now().Add(idleTimeout))
		b0, err := buf.ReadByte()
		if err != nil {
			return
		}
		b1, err := buf.ReadByte()
		if err != nil {
			return
		}
		opcode := b0 & 0x0F
		masked := (b1 & 0x80) != 0
		payloadLen := int(b1 & 0x7F)

		switch payloadLen {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(buf, ext[:]); err != nil {
				return
			}
			payloadLen = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(buf, ext[:]); err != nil {
				return
			}
			payloadLen = int(binary.BigEndian.Uint64(ext[:]))
		}

		if payloadLen > 65536 {
			return
		}

		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(buf, mask[:]); err != nil {
				return
			}
		}
		payload := make([]byte, payloadLen)
		if payloadLen > 0 {
			if _, err := io.ReadFull(buf, payload); err != nil {
				return
			}
			if masked {
				for i := range payload {
					payload[i] ^= mask[i%4]
				}
			}
		}

		switch opcode {
		case 0x8:
			closeFrame := []byte{0x88, 0x00}
			c.conn.SetWriteDeadline(time.Now().Add(200 * time.Millisecond))
			c.conn.Write(closeFrame)
			return
		case 0x9:
			pong := append([]byte{0x8A, byte(len(payload))}, payload...)
			c.enqueue(pong)
		case 0xA:
			sentAt := c.pingSentAt.Swap(0)
			if sentAt != 0 {
				rtt := time.Duration(time.Now().UnixNano() - sentAt)
				rttMs := float64(rtt) / float64(time.Millisecond)
				if c.logger != nil {
					c.logger.Printf("Latency to %s: %.1fms", c.conn.RemoteAddr(), rttMs)
				}
				c.enqueueControl(fmt.Sprintf(`{"type":"latency","rtt_ms":%.1f}`, rttMs))
			}
		}
	}
}

func (c *wsConn) Wait() {
	<-c.done
}

func (c *wsConn) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.conn.Close()
	}
}

func newOpusEncoder(sampleRate, channels, bitrate int, musicMode bool) (*opusEncoder, error) {
	var cerr C.int
	musicFlag := 0
	if musicMode {
		musicFlag = 1
	}
	ptr := C.opus_encoder_create_wrapper(C.int(sampleRate), C.int(channels), C.int(bitrate), C.int(musicFlag), &cerr)
	if ptr == nil || cerr != C.OPUS_OK {
		return nil, fmt.Errorf("opus_encoder_create failed: %d", int(cerr))
	}
	return &opusEncoder{ptr: ptr}, nil
}

func (e *opusEncoder) Encode(pcm []int16, frameSamples int, out []byte) (int, error) {
	if len(pcm) == 0 || frameSamples <= 0 {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	n := C.opus_encode_wrapper(
		e.ptr,
		(*C.opus_int16)(unsafe.Pointer(&pcm[0])),
		C.int(frameSamples),
		(*C.uchar)(unsafe.Pointer(&out[0])),
		C.int(len(out)),
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_encode: %d", int(n))
	}
	return int(n), nil
}

func (e *opusEncoder) Destroy() {
	e.mu.Lock()
	defer e.mu.Unlock()
	C.opus_encoder_destroy_wrapper(e.ptr)
}

func generateTone(pcm []int16, channels int, sampleRate int, frequency float64, phase *float64) {
	frameSamples := len(pcm) / channels
	for i := 0; i < frameSamples; i++ {
		val := int16(16384 * math.Sin(*phase))
		for ch := 0; ch < channels; ch++ {
			pcm[i*channels+ch] = val
		}
		*phase += 2 * math.Pi * frequency / float64(sampleRate)
		if *phase > 2*math.Pi {
			*phase -= 2 * math.Pi
		}
	}
}

type trafficStats struct {
	udpBytes   int64
	opusBytes  int64
	lastReport time.Time
}

func (t *trafficStats) maybeReport(hub *Hub, cfg Config, now time.Time) {
	if cfg.StatsIntervalSec <= 0 {
		return
	}
	interval := time.Duration(cfg.StatsIntervalSec) * time.Second
	elapsed := now.Sub(t.lastReport)
	if elapsed < interval {
		return
	}
	elapsedSec := elapsed.Seconds()

	sourceBps := int64(float64(t.udpBytes*8) / elapsedSec)
	opusBps := int64(float64(t.opusBytes*8) / elapsedSec)
	listeners := hub.Count()
	egressBps := opusBps * int64(listeners)

	var savingsPercent float64
	if sourceBps > 0 {
		savingsPercent = (1 - float64(opusBps)/float64(sourceBps)) * 100
		if savingsPercent < 0 {
			savingsPercent = 0
		}
	}

	msg := fmt.Sprintf(
		`{"type":"stats","source_bitrate_bps":%d,"opus_bitrate_bps":%d,"egress_bitrate_bps":%d,"savings_percent":%.1f,"listeners":%d,"channels":%d,"mode":%q}`,
		sourceBps, opusBps, egressBps, savingsPercent, listeners, cfg.Channels, cfg.Mode,
	)
	hub.BroadcastControl(msg)
	t.opusBytes = 0
	t.udpBytes = 0
	t.lastReport = now
}

func pcmListener(cfg Config, hub *Hub, logger *log.Logger) {
	frameSamples := cfg.SampleRate * cfg.FrameMS / 1000
	frameBytes := frameSamples * cfg.Channels * 2
	pcm16 := make([]int16, frameSamples*cfg.Channels)
	opusBuf := make([]byte, 4000)

	var seq uint32 = 0
	startTime := time.Now()
	frameDuration := time.Duration(cfg.FrameMS) * time.Millisecond
	stats := &trafficStats{lastReport: time.Now()}

	if cfg.TestTone {
		logger.Println("TEST TONE MODE ENABLED - Generating 440Hz sine wave")

		tonePhase := 0.0
		toneFreq := 440.0

		enc, err := newOpusEncoder(cfg.SampleRate, cfg.Channels, cfg.OpusBitrate, cfg.Mode == "music")
		if err != nil {
			logger.Fatalf("Opus encoder creation failed: %v", err)
		}
		defer enc.Destroy()

		testTicker := time.NewTicker(frameDuration)
		defer testTicker.Stop()

		logger.Printf("Starting test tone: %dHz, %dms frames, %d bps",
			int(toneFreq), cfg.FrameMS, cfg.OpusBitrate)

		for range testTicker.C {
			startLoop := time.Now()
			generateTone(pcm16, cfg.Channels, cfg.SampleRate, toneFreq, &tonePhase)

			nOut, err := enc.Encode(pcm16, frameSamples, opusBuf)
			if err != nil {
				logger.Printf("Opus encode error: %v", err)
				continue
			}

			if hub.Count() > 0 {
				ts := time.Since(startTime).Microseconds()
				buf := make([]byte, 12+nOut)
				binary.LittleEndian.PutUint32(buf[0:4], seq)
				binary.LittleEndian.PutUint64(buf[4:12], uint64(ts))
				copy(buf[12:], opusBuf[:nOut])

				hub.Broadcast(buf)
				seq++
			}

			stats.opusBytes += int64(nOut)
			stats.maybeReport(hub, cfg, time.Now())

			if cfg.DebugJitter {
				delay := time.Since(startLoop)
				if delay > frameDuration/2 {
					logger.Printf("Warning: frame processing delay %v", delay)
				}
			}
		}
		return
	}

	addr := fmt.Sprintf("%s:%d", cfg.UDPIP, cfg.PCMPort)
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		logger.Fatalf("UDP listen: %v", err)
	}
	defer conn.Close()

	if udpConn, ok := conn.(*net.UDPConn); ok {
		if err := udpConn.SetReadBuffer(262144); err != nil {
			logger.Printf("Warning: SetReadBuffer failed: %v", err)
		} else {
			logger.Printf("UDP receive buffer set to 256KB")
		}
	}

	logger.Printf("UDP PCM listener on %s", addr)
	logger.Printf("Waiting for audio on %s ... (will warn after %ds if nothing arrives)", addr, cfg.UDPWaitWarnSec)

	enc, err := newOpusEncoder(cfg.SampleRate, cfg.Channels, cfg.OpusBitrate, cfg.Mode == "music")
	if err != nil {
		logger.Fatalf("Opus encoder: %v", err)
	}
	defer enc.Destroy()

	accumulator := make([]byte, 0, frameBytes*8)
	udpBuf := make([]byte, 65536)

	var lastPacketTime time.Time
	var gapCount int
	talkerSilenceThreshold := time.Duration(cfg.SilenceThresholdMS) * time.Millisecond
	talkerActive := false
	firstPacketSeen := false
	waitWarned := false
	waitStart := time.Now()
	waitWarnAfter := time.Duration(cfg.UDPWaitWarnSec) * time.Second

	for {
		conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		n, _, err := conn.ReadFrom(udpBuf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if !firstPacketSeen && !waitWarned && waitWarnAfter > 0 && time.Since(waitStart) > waitWarnAfter {
					waitWarned = true
					logger.Printf("WARNING: no audio received on %s after %ds — check that your source (svxlink/ffmpeg/etc) is running and pointed at this address", addr, cfg.UDPWaitWarnSec)
				}
				if talkerActive && !lastPacketTime.IsZero() {
					if time.Since(lastPacketTime) > talkerSilenceThreshold {
						talkerActive = false
						logger.Printf("Talker STOP (silence detected, source: %s)", addr)
						hub.BroadcastControl(`{"type":"talker_stop"}`)
					}
				}
				if !lastPacketTime.IsZero() && cfg.DebugJitter {
					gap := time.Since(lastPacketTime)
					if gap > frameDuration*2 {
						gapCount++
						if gapCount%5 == 0 {
							logger.Printf("Audio gap: %.0fms (no data from PCM source)", float64(gap)/float64(time.Millisecond))
						}
					}
				}
				stats.maybeReport(hub, cfg, time.Now())
				continue
			}
			logger.Printf("UDP read: %v", err)
			continue
		}

		if !firstPacketSeen {
			firstPacketSeen = true
			logger.Printf("First audio packet received from %s — source is live", addr)
		}

		if !talkerActive {
			talkerActive = true
			logger.Printf("Talker START (source: %s)", addr)
			hub.BroadcastControl(`{"type":"talker_start"}`)
		}

		lastPacketTime = time.Now()
		gapCount = 0
		stats.udpBytes += int64(n)

		if len(accumulator)+n > maxAccumulatorBytes {
			logger.Printf("Accumulator overflow (%d bytes) – resetting", len(accumulator))
			accumulator = accumulator[:0]
		}

		accumulator = append(accumulator, udpBuf[:n]...)

		for len(accumulator) >= frameBytes {
			frame := accumulator[:frameBytes]

			for i := range pcm16 {
				pcm16[i] = int16(binary.LittleEndian.Uint16(frame[i*2:]))
			}

			nOut, err := enc.Encode(pcm16, frameSamples, opusBuf)
			accumulator = accumulator[frameBytes:]
			if err != nil {
				logger.Printf("Opus encode: %v", err)
				continue
			}

			if hub.Count() > 0 {
				ts := time.Since(startTime).Microseconds()

				buf := make([]byte, 12+nOut)
				binary.LittleEndian.PutUint32(buf[0:4], seq)
				binary.LittleEndian.PutUint64(buf[4:12], uint64(ts))

				copy(buf[12:], opusBuf[:nOut])

				hub.Broadcast(buf)
				seq++
			}

			stats.opusBytes += int64(nOut)
		}

		stats.maybeReport(hub, cfg, time.Now())
	}
}

func securityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		next.ServeHTTP(w, r)
	})
}

func makeLogger(path string) *log.Logger {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Fatalf("Log file: %v", err)
	}
	return log.New(io.MultiWriter(os.Stdout, f), "", log.Ldate|log.Ltime)
}

func wsHandler(cfg Config, hub *Hub, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var identity string

		if cfg.NoAuth {
			identity = "anonymous (auth disabled)"
		} else {
			token := r.URL.Query().Get("token")
			payload, err := validateJWT(token, cfg.JWTSecretPath)
			if err != nil {
				logger.Printf("Auth FAILED from %s: %v", r.RemoteAddr, err)
				http.Error(w, "unauthorized: invalid or expired token", http.StatusUnauthorized)
				return
			}
			identity = fmt.Sprintf("%s (level=%s)", payload.Email, payload.Level)
		}

		logger.Printf("Client authenticated: %s from %s", identity, r.RemoteAddr)

		if cfg.MaxClients > 0 && hub.Count() >= cfg.MaxClients {
			logger.Printf("Rejected %s: max clients (%d) reached", r.RemoteAddr, cfg.MaxClients)
			http.Error(w, "server full", http.StatusServiceUnavailable)
			return
		}

		ws, err := upgradeWS(w, r, logger)
		if err != nil {
			logger.Printf("WS upgrade error: %v", err)
			http.Error(w, "websocket required", http.StatusBadRequest)
			return
		}

		remote := ws.conn.RemoteAddr().String()
		hub.Add(ws)
		count := hub.Count()
		logger.Printf("Client connected: %s (total: %d)", remote, count)
		hub.BroadcastControl(fmt.Sprintf(`{"type":"client_count","count":%d}`, count))

		ws.Wait()

		hub.Remove(ws)
		count = hub.Count()
		logger.Printf("Client disconnected: %s (total: %d)", remote, count)
		hub.BroadcastControl(fmt.Sprintf(`{"type":"client_count","count":%d}`, count))
	}
}

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
	} else if _, err := getJWTSecret(cfg.JWTSecretPath); err != nil {
		logger.Printf("WARNING: JWT secret file not found: %v", err)
		logger.Printf("Please ensure %s exists and is readable", cfg.JWTSecretPath)
	}

	hub := NewHub()
	go pcmListener(*cfg, hub, logger)

	mux := http.NewServeMux()
	mux.Handle("/", securityMiddleware(http.HandlerFunc(wsHandler(*cfg, hub, logger))))

	srv := &http.Server{
		Addr:    ":" + cfg.WSPort,
		Handler: mux,
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
