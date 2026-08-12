const SOCKET_URL = (typeof window.AUDIO_SOCKET_URL !== 'undefined' && window.AUDIO_SOCKET_URL)
    ? window.AUDIO_SOCKET_URL
    : 'wss://example.net:8080/';

const AUTH_TOKEN = (typeof window.AUDIO_TOKEN !== 'undefined' && window.AUDIO_TOKEN)
    ? window.AUDIO_TOKEN
    : '';

const WS_URL = SOCKET_URL + (SOCKET_URL.includes('?') ? '&' : '?') + 'token=' + encodeURIComponent(AUTH_TOKEN);
const SAMPLE_RATE    = 48000;
const CHANNELS       = (typeof window.AUDIO_CHANNELS !== 'undefined' && window.AUDIO_CHANNELS)
    ? window.AUDIO_CHANNELS
    : 1;
const FRAME_DURATION = 0.02;

// WebCodecs (AudioDecoder) compatibility layer for browsers/OSes that
// don't ship it natively (e.g. older Windows builds still on outdated
// Chromium-based browsers). Only ever downloaded lazily, the first time
// startPlayer() runs on a client where 'AudioDecoder' is missing — modern
// browsers never pay this extra request. Both can be overridden from the
// host page, before this script is loaded:
//   window.AUDIO_ENABLE_POLYFILL = false;               // opt out entirely
//   window.AUDIO_POLYFILL_URL    = '/vendor/polyfill.js'; // self-hosted copy
const ENABLE_POLYFILL = (typeof window.AUDIO_ENABLE_POLYFILL !== 'undefined')
    ? !!window.AUDIO_ENABLE_POLYFILL
    : true;
const POLYFILL_URL = (typeof window.AUDIO_POLYFILL_URL !== 'undefined' && window.AUDIO_POLYFILL_URL)
    ? window.AUDIO_POLYFILL_URL
    : 'https://cdn.jsdelivr.net/npm/libavjs-webcodecs-polyfill@0.0.3/dist/libavjs-webcodecs-polyfill.min.js';

let polyfillLoaded  = false;
let polyfillLoading = false;

let ws           = null;
let audioCtx     = null;
let isPlaying    = false;
let gainNode     = null;
let opusDecoder  = null;
let nextPlayTime = 0;
// AudioBufferSourceNodes scheduled via src.start() but not yet confirmed
// finished. Per the Web Audio spec, a source node isn't eligible for GC
// until it has actually played to completion. If audioCtx.currentTime
// stops advancing for a long stretch (e.g. some browsers throttle/suspend
// audio processing in a minimized/backgrounded tab) while WS frames keep
// arriving, nodes would otherwise pile up here forever. See
// resetPlaybackClock() below, which actively stops stale ones instead of
// just abandoning the reference.
let pendingSources = [];
let timestamp    = 0;
let lastRttMs    = null;
let lastStats    = null; // {opus_bitrate_bps, udp_bitrate_bps, listeners, channels, mode}

const style = document.createElement('style');
style.textContent = `
    @keyframes pulse {
        0% { opacity: 1; transform: scale(1); }
        50% { opacity: 0.5; transform: scale(0.8); }
        100% { opacity: 1; transform: scale(1); }
    }
`;
document.head.appendChild(style);

function formatBps(bps) {
    if (bps >= 1000000) return (bps / 1000000).toFixed(1) + 'Mbps';
    if (bps >= 1000) return Math.round(bps / 1000) + 'kbps';
    return bps + 'bps';
}

function connectedTooltip() {
    const parts = [];
    if (lastRttMs !== null) parts.push(`${lastRttMs}ms`);
    if (lastStats) {
        parts.push(`${formatBps(lastStats.opus_bitrate_bps)} opus`);
        if (lastStats.savings_percent > 0) {
            parts.push(`${Math.round(lastStats.savings_percent)}% saved`);
        }
    }
    return parts.length ? `Connected — ${parts.join(' · ')}` : 'Connected';
}

// Resolves once AudioDecoder is usable — either natively, or (if enabled)
// after loading the WASM-based libavjs-webcodecs-polyfill fallback.
// Rejects only if AudioDecoder is unavailable AND the polyfill is either
// disabled or fails to load/init.
function ensurePolyfillLoaded() {
    return new Promise((resolve, reject) => {
        if ('AudioDecoder' in window) {
            resolve();
            return;
        }

        if (!ENABLE_POLYFILL) {
            reject(new Error('AudioDecoder not supported and polyfill is disabled'));
            return;
        }

        if (polyfillLoaded) {
            resolve();
            return;
        }

        if (polyfillLoading) {
            const check = setInterval(() => {
                if (polyfillLoaded) {
                    clearInterval(check);
                    resolve();
                }
            }, 200);
            return;
        }

        polyfillLoading = true;

        const script = document.createElement('script');
        script.src = POLYFILL_URL;
        script.crossOrigin = 'anonymous';

        script.onload = () => {
            if (typeof LibAVWebCodecs === 'undefined') {
                polyfillLoading = false;
                reject(new Error('LibAVWebCodecs not found after loading ' + POLYFILL_URL));
                return;
            }
            LibAVWebCodecs.load({ polyfill: true })
                .then(() => {
                    polyfillLoaded = true;
                    polyfillLoading = false;
                    resolve();
                })
                .catch((err) => {
                    polyfillLoading = false;
                    reject(new Error('Polyfill init failed: ' + err.message));
                });
        };

        script.onerror = () => {
            polyfillLoading = false;
            reject(new Error('Failed to load polyfill script from ' + POLYFILL_URL));
        };

        document.head.appendChild(script);
    });
}

// Stops and drops references to every AudioBufferSourceNode scheduled
// via src.start() that hasn't already finished playing, then resets the
// playback clock. Used on every timeline discontinuity (startup, a
// silence/talker gap, reconnect, or normal drift correction) so stale
// nodes never accumulate — see the pendingSources comment above.
function resetPlaybackClock(atTime) {
    for (const p of pendingSources) {
        try { p.src.stop(0); } catch (e) {}
        try { p.src.disconnect(); } catch (e) {}
    }
    pendingSources = [];
    nextPlayTime = atTime;
}

function updateConnectionStatus(status) {
    const toggleBtn = document.getElementById('togglePlayer');
    let statusIndicator = document.getElementById('ws-status-indicator');
    
    if (!statusIndicator) {
        const indicator = document.createElement('span');
        indicator.id = 'ws-status-indicator';
        indicator.style.display = 'inline-block';
        indicator.style.width = '6px';
        indicator.style.height = '6px';
        indicator.style.borderRadius = '50%';
        indicator.style.marginLeft = '6px';
        indicator.style.backgroundColor = '#ccc';
        indicator.style.boxShadow = '0 0 4px currentColor';
        toggleBtn.appendChild(indicator);
        statusIndicator = indicator;
    }
    
    statusIndicator.style.animation = '';
    
    switch(status) {
        case 'connecting':
            statusIndicator.style.backgroundColor = '#ff9800';
            statusIndicator.style.boxShadow = '0 0 6px #ff9800';
            statusIndicator.title = 'Connecting...';
            break;
        case 'connected':
            statusIndicator.style.backgroundColor = 'rgb(152, 255, 156)';
            statusIndicator.style.boxShadow = '0 0 6px #4caf50';
            statusIndicator.title = connectedTooltip();
            break;
        case 'disconnected':
            statusIndicator.style.backgroundColor = '#f44336';
            statusIndicator.style.boxShadow = '0 0 6px #f44336';
            statusIndicator.title = 'Disconnected';
            break;
        case 'reconnecting':
            statusIndicator.style.backgroundColor = '#ff9800';
            statusIndicator.style.animation = 'pulse 1s infinite';
            statusIndicator.title = 'Reconnecting...';
            break;
        default:
            statusIndicator.style.backgroundColor = '#ccc';
            statusIndicator.style.boxShadow = 'none';
            statusIndicator.title = '';
    }
}

document.getElementById('togglePlayer').addEventListener('click', function () {
    if (isPlaying) {
        stopPlayer();
        this.textContent = 'Monitor OFF';
        this.className   = 'btn btn-danger btn-xs';
    } else {
        startPlayer();
        this.textContent = 'Monitor ON';
        this.className   = 'btn btn-success btn-xs';
    }
});

async function startPlayer() {
    if (isPlaying) return;

    updateConnectionStatus('connecting'); // reused as a lightweight "please wait" indicator while the polyfill loads
    try {
        await ensurePolyfillLoaded();
    } catch (err) {
        updateConnectionStatus('disconnected');
        alert('Your browser does not support WebCodecs (AudioDecoder) and the compatibility layer could not be loaded.\n\n' + err.message);
        return;
    }

    if (!('AudioDecoder' in window)) {
        updateConnectionStatus('disconnected');
        alert('Your browser does not support WebCodecs, even with the compatibility layer.');
        return;
    }

    audioCtx = new (window.AudioContext || window.webkitAudioContext)({
        sampleRate: SAMPLE_RATE,
        latencyHint: 'playback'
    });

    if (audioCtx.state === 'suspended') {
        await audioCtx.resume();
    }

    gainNode = audioCtx.createGain();
    gainNode.gain.value = 1.0;
    gainNode.connect(audioCtx.destination);

    resetPlaybackClock(0);
    timestamp    = 0;
    isPlaying    = true;

    initDecoder();
    connectWebSocket();
}

function initDecoder() {
    if (opusDecoder && opusDecoder.state !== 'closed') {
        try { opusDecoder.close(); } catch(e) {}
        opusDecoder = null;
    }

    opusDecoder = new AudioDecoder({
        output: (audioData) => {
            if (!isPlaying || !audioCtx || !gainNode) {
                audioData.close();
                return;
            }

            const nFrames = audioData.numberOfFrames;
            const ab = audioCtx.createBuffer(CHANNELS, nFrames, SAMPLE_RATE);
            const buf = new Float32Array(nFrames);
            for (let ch = 0; ch < CHANNELS; ch++) {
                audioData.copyTo(buf, { planeIndex: ch, format: 'f32-planar' });
                ab.copyToChannel(buf, ch);
            }
            audioData.close();

            const now = audioCtx.currentTime;

            if (nextPlayTime < now || nextPlayTime - now > 0.3) {
                // Discontinuity: either we fell behind (gap/reconnect) or
                // drifted too far ahead. In the drift case specifically,
                // previously scheduled nodes may never actually reach
                // their start time (see pendingSources comment) — stop
                // them explicitly rather than just moving nextPlayTime.
                resetPlaybackClock(now + 0.10);
            }

            const src = audioCtx.createBufferSource();
            src.buffer = ab;
            src.connect(gainNode);
            src.start(nextPlayTime);
            const startAt = nextPlayTime;
            nextPlayTime += ab.duration;
            pendingSources.push({ src, endAt: startAt + ab.duration });

            // Opportunistic cleanup during normal playback so the array
            // doesn't grow unbounded between discontinuities either.
            if (pendingSources.length > 64) {
                pendingSources = pendingSources.filter(p => p.endAt > now);
            }
        },
        error: (e) => {
            if (isPlaying) initDecoder();
        }
    });

    try {
        opusDecoder.configure({
            codec:            'opus',
            sampleRate:       SAMPLE_RATE,
            numberOfChannels: CHANNELS,
        });
    } catch (e) {
        console.warn('Decoder config error:', e);
    }
}

function connectWebSocket() {
    if (ws) {
        try { ws.close(); } catch(e) {}
        ws = null;
    }

    updateConnectionStatus('connecting');
    ws = new WebSocket(WS_URL);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => {
        updateConnectionStatus('connected');
    };

    ws.onmessage = (event) => {
        if (!isPlaying) return;

        if (typeof event.data === 'string') {
            try {
                const msg = JSON.parse(event.data);
                if (msg.type === 'talker_start') {
                    resetPlaybackClock(0);
                } else if (msg.type === 'talker_stop') {
                    resetPlaybackClock(0);
                } else if (msg.type === 'client_count') {
                    updateListenerCount(msg.count);
                } else if (msg.type === 'latency') {
                    lastRttMs = Math.round(msg.rtt_ms);
                    updateConnectionStatus('connected');
                } else if (msg.type === 'stats') {
                    lastStats = msg;
                    updateConnectionStatus('connected');
                }
            } catch(e) {}
            return;
        }

        if (!opusDecoder || opusDecoder.state !== 'configured') return;
        if (!event.data || event.data.byteLength < 12) return;

        const opusData = new Uint8Array(event.data, 12);
        const chunk = new EncodedAudioChunk({
            type:      'key',
            timestamp: timestamp,
            data:      opusData
        });
        timestamp += FRAME_DURATION * 1e6;

        try {
            opusDecoder.decode(chunk);
        } catch(e) {}
    };

    ws.onerror = (e) => {
        updateConnectionStatus('disconnected');
    };

    ws.onclose = (e) => {
        if (!isPlaying) {
            updateConnectionStatus('disconnected');
            return;
        }

        if (e.code === 4001) {
            updateConnectionStatus('disconnected');
            alert('Authentication failed');
            if (isPlaying) document.getElementById('togglePlayer').click();
            return;
        }
        
        resetPlaybackClock(0);
        timestamp    = 0;

        updateConnectionStatus('reconnecting');
        const reconnectDelay = 2000 + Math.random() * 1000;
        setTimeout(() => {
            if (isPlaying) {
                initDecoder();
                connectWebSocket();
            }
        }, reconnectDelay);
    };
}

function updateListenerCount(count) {
    const btn = document.getElementById('togglePlayer');
    if (!isPlaying || !btn) return;
    const indicator = document.getElementById('ws-status-indicator');
    btn.childNodes.forEach(node => {
        if (node.nodeType === Node.TEXT_NODE) node.remove();
    });
    btn.insertBefore(
        document.createTextNode(`Monitor ON (${count})`),
        btn.firstChild
    );
}

function stopPlayer() {
    isPlaying = false;

    if (ws) {
        try { ws.close(); } catch(e) {}
        ws = null;
    }

    if (opusDecoder && opusDecoder.state !== 'closed') {
        try { opusDecoder.close(); } catch(e) {}
        opusDecoder = null;
    }

    if (gainNode) {
        try { gainNode.disconnect(); } catch(e) {}
        gainNode = null;
    }

    if (audioCtx) {
        try { audioCtx.close(); } catch(e) {}
        audioCtx = null;
    }

    pendingSources = [];
    nextPlayTime = 0;
    lastRttMs = null;
    lastStats = null;

    updateConnectionStatus('disconnected');

}