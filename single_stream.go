package brotli

import (
	"errors"
	"io"
)

var (
	// ErrStreamNotFinished is returned by Reader.TrailingReader when the
	// Brotli stream has not been completely decoded yet. The caller must keep
	// calling Read until it gets io.EOF before asking for the trailing data.
	ErrStreamNotFinished = errors.New("brotli: stream has not finished decoding")

	// ErrTrailingDataUnavailable is returned by Reader.TrailingReader when the
	// trailing data has already been handed out, when the Reader is still in
	// strict mode, or when the stream was truncated or corrupt.
	ErrTrailingDataUnavailable = errors.New("brotli: trailing data is not available")
)

// NewSingleStreamReader creates a new Reader in single-stream mode.
//
// In single-stream mode the Reader decodes exactly one Brotli stream and then
// reports io.EOF, even if the underlying reader contains additional bytes.
// Once the stream has finished, TrailingReader provides access to whatever
// follows it: first any bytes that were already pulled into the Reader's
// internal read-ahead buffer, and then the remaining bytes of src. The caller
// therefore does not have to account for how far src has already advanced.
//
// The trailing data is not inspected, and in particular bytes that happen to
// start another valid Brotli stream are not decoded or concatenated.
//
// NewReader keeps the historical strict behavior: any byte present after the
// end of the Brotli stream makes Read fail with an "excessive input" error.
func NewSingleStreamReader(src io.Reader) *Reader {
	r := NewReader(src)
	r.singleStream = true
	return r
}

// TrailingReader returns a Reader for the bytes that follow the Brotli stream.
// It must be called after decoding has finished, that is after Read returned
// io.EOF in single-stream mode.
//
// The returned Reader first yields the bytes the decoder already read ahead
// but did not consume, and then continues reading from the original source.
// TrailingReader may only be called once per decoded stream; after another
// call to Reset it must be obtained again.
//
// It returns ErrStreamNotFinished if the stream has not been fully decoded,
// and ErrTrailingDataUnavailable if the trailing data was already taken, if
// the Reader is in strict mode, or if the stream was truncated or corrupt.
func (r *Reader) TrailingReader() (io.Reader, error) {
	if r.error_code < 0 || r.decodeFailed {
		return nil, ErrTrailingDataUnavailable
	}
	if !r.singleStream || !r.streamDone {
		return nil, ErrStreamNotFinished
	}
	if r.trailingTaken {
		return nil, ErrTrailingDataUnavailable
	}
	r.trailingTaken = true

	tail := r.tailBytes
	r.tailBytes = nil

	return &trailingReader{
		tail: tail,
		src:  r.src,
	}, nil
}

// trailingReader hands back bytes that were read ahead by the decoder before
// transparently continuing on the underlying source.
type trailingReader struct {
	tail []byte
	src  io.Reader
}

func (t *trailingReader) Read(p []byte) (int, error) {
	if len(t.tail) > 0 {
		n := copy(p, t.tail)
		t.tail = t.tail[n:]
		return n, nil
	}
	if t.src == nil {
		return 0, io.EOF
	}
	return t.src.Read(p)
}
