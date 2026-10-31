//go:build darwin && cgo

// Microphone capture through AudioQueue, mono int16 at the requested rate.
//
// The queue converts from the device's native rate itself, delivers buffers on
// its own thread, and follows the default input device. The callback copies
// each buffer into a ring that Go polls, so nothing here ever calls into Go
// and nothing in Go ever blocks in here.

#include <AudioToolbox/AudioToolbox.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>

#include "aq_darwin.h"

#define T2_BUFFERS 4
#define T2_MIN_RING 4096 /* frames: 256ms at 16kHz, however small the buffers */

struct t2_stream {
	AudioQueueRef q;
	pthread_mutex_t mu;
	int16_t *ring;
	size_t cap, len, head;
	int overflows;
};

static void t2_input_cb(void *ud, AudioQueueRef q, AudioQueueBufferRef b,
                        const AudioTimeStamp *ts, UInt32 n,
                        const AudioStreamPacketDescription *d) {
	t2_stream *s = (t2_stream *)ud;
	size_t samples = b->mAudioDataByteSize / sizeof(int16_t);
	const int16_t *src = (const int16_t *)b->mAudioData;

	pthread_mutex_lock(&s->mu);
	if (s->len + samples > s->cap) {
		s->overflows++;
		samples = s->cap - s->len; /* keep what fits, drop the rest */
	}
	for (size_t i = 0; i < samples; i++) {
		s->ring[(s->head + s->len) % s->cap] = src[i];
		s->len++;
	}
	pthread_mutex_unlock(&s->mu);

	/* Hand the buffer straight back; a queue that runs out of buffers drops
	   audio silently. After a stop this fails harmlessly. */
	AudioQueueEnqueueBuffer(q, b, 0, NULL);
}

static void t2_free(t2_stream *s) {
	pthread_mutex_destroy(&s->mu);
	free(s->ring);
	free(s);
}

t2_stream *t2_stream_open(int sampleRate, int frames, int *status) {
	t2_stream *s = calloc(1, sizeof(*s));
	if (s == NULL) {
		*status = T2_ERR_NOMEM;
		return NULL;
	}
	pthread_mutex_init(&s->mu, NULL);
	s->cap = (size_t)frames * 16;
	if (s->cap < T2_MIN_RING) {
		s->cap = T2_MIN_RING;
	}
	s->ring = calloc(s->cap, sizeof(int16_t));
	if (s->ring == NULL) {
		*status = T2_ERR_NOMEM;
		t2_free(s);
		return NULL;
	}

	AudioStreamBasicDescription fmt;
	memset(&fmt, 0, sizeof fmt);
	fmt.mSampleRate = sampleRate;
	fmt.mFormatID = kAudioFormatLinearPCM;
	fmt.mFormatFlags = kLinearPCMFormatFlagIsSignedInteger | kLinearPCMFormatFlagIsPacked;
	fmt.mBitsPerChannel = 16;
	fmt.mChannelsPerFrame = 1;
	fmt.mBytesPerFrame = 2;
	fmt.mFramesPerPacket = 1;
	fmt.mBytesPerPacket = 2;

	/* No run loop: the callback runs on the queue's internal thread. */
	OSStatus st = AudioQueueNewInput(&fmt, t2_input_cb, s, NULL, kCFRunLoopCommonModes, 0, &s->q);
	if (st != noErr) {
		*status = (int)st;
		t2_free(s);
		return NULL;
	}

	for (int i = 0; i < T2_BUFFERS; i++) {
		AudioQueueBufferRef b;
		st = AudioQueueAllocateBuffer(s->q, (UInt32)frames * 2, &b);
		if (st == noErr) {
			st = AudioQueueEnqueueBuffer(s->q, b, 0, NULL);
		}
		if (st != noErr) {
			*status = (int)st;
			AudioQueueDispose(s->q, true);
			t2_free(s);
			return NULL;
		}
	}

	st = AudioQueueStart(s->q, NULL);
	if (st != noErr) {
		*status = (int)st;
		AudioQueueDispose(s->q, true);
		t2_free(s);
		return NULL;
	}

	*status = 0;
	return s;
}

int t2_stream_available(t2_stream *s) {
	pthread_mutex_lock(&s->mu);
	int n = (int)s->len;
	pthread_mutex_unlock(&s->mu);
	return n;
}

int t2_stream_read(t2_stream *s, int16_t *out, int n) {
	pthread_mutex_lock(&s->mu);
	int got = 0;
	while (got < n && s->len > 0) {
		out[got++] = s->ring[s->head];
		s->head = (s->head + 1) % s->cap;
		s->len--;
	}
	pthread_mutex_unlock(&s->mu);
	return got;
}

int t2_stream_overflows(t2_stream *s) {
	pthread_mutex_lock(&s->mu);
	int n = s->overflows;
	pthread_mutex_unlock(&s->mu);
	return n;
}

int t2_stream_close(t2_stream *s) {
	/* Both synchronous: once Dispose returns the callback cannot run again,
	   so the ring can be freed. */
	OSStatus st = AudioQueueStop(s->q, true);
	OSStatus dt = AudioQueueDispose(s->q, true);
	t2_free(s);
	return (int)(st != noErr ? st : dt);
}
