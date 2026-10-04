package brotli

import (
	"errors"
	"io"

	"github.com/andybalholm/brotli/matchfinder"
)

const (
	BestSpeed          = 0
	BestCompression    = 11
	DefaultCompression = 6
)

// WriterOptions configures Writer.
type WriterOptions struct {
	// Quality controls the compression-speed vs compression-density trade-offs.
	// The higher the quality, the slower the compression. Range is 0 to 11.
	Quality int
	// LGWin is the base 2 logarithm of the sliding window size.
	// Range is 10 to 24. 0 indicates automatic configuration based on Quality.
	LGWin int
	// Bounded selects bounded encoding mode: the writer guarantees that the
	// total output for n payload bytes and f calls to Flush never exceeds
	// MaxEncodedSize(n, options, f) bytes. Bounded mode only changes the
	// behavior of the quality 0 and 1 fast paths, which otherwise emit one
	// meta-block per Write call: input is buffered and emitted in segments
	// of 1<<LGWin bytes so that the meta-block count depends only on the
	// total input size and the number of flushes. For quality 2 and above
	// the encoder already satisfies the bound and Bounded is a no-op.
	Bounded bool
}

var (
	errEncode       = errors.New("brotli: encode error")
	errWriterClosed = errors.New("brotli: Writer is closed")
)

// Writes to the returned writer are compressed and written to dst.
// It is the caller's responsibility to call Close on the Writer when done.
// Writes may be buffered and not flushed until Close.
func NewWriter(dst io.Writer) *Writer {
	return NewWriterLevel(dst, DefaultCompression)
}

// NewWriterLevel is like NewWriter but specifies the compression level instead
// of assuming DefaultCompression.
// The compression level can be DefaultCompression or any integer value between
// BestSpeed and BestCompression inclusive.
func NewWriterLevel(dst io.Writer, level int) *Writer {
	return NewWriterOptions(dst, WriterOptions{
		Quality: level,
	})
}

// NewWriterOptions is like NewWriter but specifies WriterOptions
func NewWriterOptions(dst io.Writer, options WriterOptions) *Writer {
	w := new(Writer)
	w.options = options
	w.Reset(dst)
	return w
}

// Reset discards the Writer's state and makes it equivalent to the result of
// its original state from NewWriter or NewWriterLevel, but writing to dst
// instead. This permits reusing a Writer rather than allocating a new one.
func (w *Writer) Reset(dst io.Writer) {
	encoderInitState(w)
	w.params.quality = w.options.Quality
	if w.options.LGWin > 0 {
		w.params.lgwin = uint(w.options.LGWin)
	}
	w.dst = dst
	w.err = nil
}

func (w *Writer) writeChunk(p []byte, op int) (n int, err error) {
	if w.dst == nil {
		return 0, errWriterClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.options.Bounded && sanitizedQuality(w.options.Quality) <= fastTwoPassCompressionQuality {
		return w.writeChunkBounded(p, op)
	}

	for {
		availableIn := uint(len(p))
		nextIn := p
		success := encoderCompressStream(w, op, &availableIn, &nextIn)
		bytesConsumed := len(p) - int(availableIn)
		p = p[bytesConsumed:]
		n += bytesConsumed
		if !success {
			return n, errEncode
		}

		if len(p) == 0 || w.err != nil {
			return n, w.err
		}
	}
}

// writeChunkBounded implements bounded encoding mode for quality levels 0
// and 1. The fast fragment encoder emits one meta-block per
// encoderCompressStream call, so without buffering the meta-block count
// (and with it the per-block header overhead) would depend on the caller's
// Write pattern. Here input is accumulated and emitted in segments of one
// block_size_limit (1<<LGWin) bytes, so the number of emitted meta-blocks
// is at most size/(1<<LGWin) + flushes + 1 regardless of the Write pattern.
// Each segment is consumed by a single encoderCompressStream call, and the
// fragment encoder's uncompressed-block fallback keeps every emitted
// meta-block within maxMetaBlockOverheadBits bits on top of its payload.
func (w *Writer) writeChunkBounded(p []byte, op int) (n int, err error) {
	n = len(p)
	segSize := 1 << uint(sanitizedLGWin(w.options.LGWin))

	// Emit full segments straight from p while the buffer is empty.
	for len(w.segBuf) == 0 && len(p) >= segSize {
		if !w.emitBoundedSegment(p[:segSize], operationProcess) {
			return n, errEncode
		}
		p = p[segSize:]
	}
	if len(p) > 0 {
		w.segBuf = append(w.segBuf, p...)
		consumed := 0
		for len(w.segBuf)-consumed >= segSize {
			if !w.emitBoundedSegment(w.segBuf[consumed:][:segSize], operationProcess) {
				return n, errEncode
			}
			consumed += segSize
		}
		if consumed > 0 {
			w.segBuf = append(w.segBuf[:0], w.segBuf[consumed:]...)
		}
	}

	switch op {
	case operationFlush:
		// Emit the buffered partial segment, then the byte-alignment
		// padding block.
		if len(w.segBuf) > 0 {
			if !w.emitBoundedSegment(w.segBuf, operationProcess) {
				return n, errEncode
			}
			w.segBuf = w.segBuf[:0]
		}
		if !w.emitBoundedSegment(nil, operationFlush) {
			return n, errEncode
		}
	case operationFinish:
		// Emit the remaining partial segment as the last meta-block (or an
		// empty last meta-block if nothing is buffered).
		if !w.emitBoundedSegment(w.segBuf, operationFinish) {
			return n, errEncode
		}
		w.segBuf = w.segBuf[:0]
	}
	return n, w.err
}

// emitBoundedSegment feeds one segment (at most one block_size_limit) to
// the encoder in a single call and verifies that it is fully consumed, so
// that exactly one meta-block is emitted per segment.
func (w *Writer) emitBoundedSegment(seg []byte, op int) bool {
	if w.err != nil {
		return false
	}
	availableIn := uint(len(seg))
	nextIn := seg
	if !encoderCompressStream(w, op, &availableIn, &nextIn) {
		return false
	}
	return availableIn == 0 && w.err == nil
}

// Flush outputs encoded data for all input provided to Write. The resulting
// output can be decoded to match all input before Flush, but the stream is
// not yet complete until after Close.
// Flush has a negative impact on compression.
func (w *Writer) Flush() error {
	_, err := w.writeChunk(nil, operationFlush)
	return err
}

// Close flushes remaining data to the decorated writer.
func (w *Writer) Close() error {
	// If stream is already closed, it is reported by `writeChunk`.
	_, err := w.writeChunk(nil, operationFinish)
	w.dst = nil
	return err
}

// Write implements io.Writer. Flush or Close must be called to ensure that the
// encoded bytes are actually flushed to the underlying Writer.
func (w *Writer) Write(p []byte) (n int, err error) {
	return w.writeChunk(p, operationProcess)
}

type nopCloser struct {
	io.Writer
}

func (nopCloser) Close() error { return nil }

// NewWriterV2 is like NewWriterLevel, but it uses the new implementation
// based on the matchfinder package. It currently supports up to level 9;
// if a higher level is specified, level 9 will be used.
func NewWriterV2(dst io.Writer, level int) *matchfinder.Writer {
	if level < 0 {
		level = 0
	} else if level > 9 {
		level = 9
	}
	var mf matchfinder.MatchFinder
	switch level {
	case 0, 1:
		mf = &matchfinder.ZFast{MaxDistance: 1 << 20}
	case 2:
		mf = &matchfinder.ZDFast{MaxDistance: 1 << 20}
	case 3:
		mf = &matchfinder.ZM{MaxDistance: 1 << 20}
	case 4:
		mf = &matchfinder.Trio{MaxDistance: 1 << 20}
	case 5, 6:
		mf = &matchfinder.Bargain1{MaxDistance: 1 << 20}
	case 7:
		mf = &matchfinder.Bargain2{MaxDistance: 1 << 20, Skip: true}
	case 8:
		mf = &matchfinder.Bargain2{MaxDistance: 1 << 20}
	case 9:
		mf = &matchfinder.Bargain3{MaxDistance: 1 << 20}
	}

	w := &matchfinder.Writer{
		Dest:        dst,
		MatchFinder: mf,
		Encoder:     &Encoder{},
		BlockSize:   1 << 16,
	}
	if level < 1 {
		w.Encoder = &FastEncoder{}
	}
	return w
}
