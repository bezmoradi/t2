#include <stdint.h>

#define T2_ERR_NOMEM (-108)

typedef struct t2_stream t2_stream;

t2_stream *t2_stream_open(int sampleRate, int frames, int *status);
int t2_stream_available(t2_stream *s);
int t2_stream_read(t2_stream *s, int16_t *out, int n);
int t2_stream_overflows(t2_stream *s);
int t2_stream_close(t2_stream *s);
