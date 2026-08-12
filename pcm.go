package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"net"
	"time"
)

const maxAccumulatorBytes = 48000 * 2 * 4

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
		_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond)) // best-effort; a failure here just surfaces on ReadFrom below
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
