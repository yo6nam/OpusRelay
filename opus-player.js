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

let ws           = null;
let audioCtx     = null;
let isPlaying    = false;
let gainNode     = null;
let peakMeter    = null;
let opusDecoder  = null;
let nextPlayTime = 0;
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

    $('#peak-meter').slideDown();

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

    if (typeof webAudioPeakMeter !== 'undefined') {
        try {
            const PeakMeterClass = webAudioPeakMeter.WebAudioPeakMeter;
            if (PeakMeterClass) {
                peakMeter = new PeakMeterClass(gainNode, document.getElementById('peak-meter'), {
                    backgroundColor: 'rgba(2, 50, 98, 0.77)',
                    borderSize: 2,
                    tickColor: '#ddd',
                    labelColor: '#ddd',
                    gradient: ['red 1%', '#ff0 16%', 'lime 45%', '#080 100%'],
                    dbRangeMin: -42,
                    dbRangeMax: 0,
                    dbTickSize: 6,
                    vertical: false,
                    fontSize: 9,
                    maskTransition: '0.05s',
                    audioMeterStandard: 'peak-sample',
                    peakHoldDuration: 1200
                });
            }
        } catch (e) {}
    }

    if (!('AudioDecoder' in window)) {
        alert('Your browser does not support WebCodecs.');
        return;
    }

    nextPlayTime = 0;
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
                nextPlayTime = now + 0.10;
            }

            const src = audioCtx.createBufferSource();
            src.buffer = ab;
            src.connect(gainNode);
            src.start(nextPlayTime);
            nextPlayTime += ab.duration;
        },
        error: (e) => {
            console.warn('AudioDecoder error:', e);
            if (isPlaying) initDecoder();
        }
    });

    opusDecoder.configure({
        codec:            'opus',
        sampleRate:       SAMPLE_RATE,
        numberOfChannels: CHANNELS,
    });
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
        console.log('✅ WebSocket connected');
        updateConnectionStatus('connected');
    };

    ws.onmessage = (event) => {
        if (!isPlaying) return;

        if (typeof event.data === 'string') {
            try {
                const msg = JSON.parse(event.data);
                if (msg.type === 'talker_start') {
                    nextPlayTime = 0;
                } else if (msg.type === 'talker_stop') {
                    nextPlayTime = 0;
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
        console.error('WebSocket error:', e);
        updateConnectionStatus('disconnected');
    };

    ws.onclose = (e) => {
        console.log('WebSocket closed:', e.code, e.reason);
        
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
        
        nextPlayTime = 0;
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

    nextPlayTime = 0;
    lastRttMs = null;
    lastStats = null;

    updateConnectionStatus('disconnected');

    $('#peak-meter').slideUp(function() {
        if (peakMeter) {
            try { peakMeter.cleanup(); } catch(e) {}
            peakMeter = null;
        }
    });
}