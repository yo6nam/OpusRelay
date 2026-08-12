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
	"fmt"
	"sync"
	"unsafe"
)

type opusEncoder struct {
	ptr *C.GoOpusEncoder
	mu  sync.Mutex
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
	if len(pcm) == 0 || frameSamples <= 0 || len(out) == 0 {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// #nosec G103 -- unavoidable for cgo: opus_encode needs raw pointers into
	// the Go-owned pcm/out buffers. Safe here because both len(pcm)==0 and
	// len(out)==0 are already rejected above, so neither &pcm[0] nor
	// &out[0] can run on an empty slice.
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
