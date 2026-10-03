package brotli

import (
	"errors"
	"io"
)

type decodeError int

func (err decodeError) Error() string {
	return "brotli: " + string(decoderErrorString(int(err)))
}

var errExcessiveInput = errors.New("brotli: excessive input")
var errInvalidState = errors.New("brotli: invalid state")

// errTrailingNotReady is returned by Reader.TrailingData before the Brotli
// stream has been fully decoded.
var errTrailingNotReady = errors.New("brotli: TrailingData called before end of stream")
var errTrailingNotSingleStream = errors.New("brotli: TrailingData requires SingleStream mode")

// Sentinel errors returned by the Reader, exported so callers can detect
// them with errors.Is.
var (
	// ErrExcessiveInput is returned in strict mode when the source contains
	// data after the end of the Brotli stream.
	ErrExcessiveInput = errExcessiveInput
	// ErrTrailingDataNotReady is returned by TrailingData before the Brotli
	// stream has ended.
	ErrTrailingDataNotReady = errTrailingNotReady
	// ErrTrailingDataUnsupported is returned by TrailingData when the Reader
	// was not created in SingleStream mode.
	ErrTrailingDataUnsupported = errTrailingNotSingleStream
)

// readBufSize is a "good" buffer size that avoids excessive round-trips
// between C and Go but doesn't waste too much memory on buffering.
// It is arbitrarily chosen to be equal to the constant used in io.Copy.
const readBufSize = 32 * 1024

// ReaderOptions configures a Reader.
type ReaderOptions struct {
	// SingleStream limits the Reader to decoding exactly one Brotli stream.
	//
	// By default (SingleStream == false) the Reader operates in strict mode:
	// the underlying source must contain exactly one Brotli stream and any
	// byte left over after the stream finishes causes Read to return
	// ErrExcessiveInput.
	//
	// In single-stream mode decoding ends at the boundary of the current
	// Brotli stream. After Read signals the end of the stream (a read that
	// returns io.EOF), TrailingData returns a Reader over the bytes that
	// followed the compressed data, including any bytes that were already
	// pre-read into the Reader's internal buffer. The trailing bytes are not
	// examined or parsed: if they happen to begin another Brotli stream it is
	// not decoded or merged automatically.
	SingleStream bool
}

// NewReader creates a new Reader reading the given reader.
func NewReader(src io.Reader) *Reader {
	r := new(Reader)
	r.Reset(src)
	return r
}

// NewReaderOptions is like NewReader but specifies ReaderOptions.
func NewReaderOptions(src io.Reader, options ReaderOptions) *Reader {
	r := new(Reader)
	r.ResetReader(src, options)
	return r
}

// Reset discards the Reader's state and makes it equivalent to the result of
// its original state from NewReader, but reading from src instead.
// This permits reusing a Reader rather than allocating a new one.
// Error is always nil
func (r *Reader) Reset(src io.Reader) error {
	return r.ResetReader(src, ReaderOptions{})
}

// ResetReader discards the Reader's state and reinitializes it to read from
// src using the given options. It is the configurable form of Reset.
// Error is always nil
func (r *Reader) ResetReader(src io.Reader, options ReaderOptions) error {
	if r.error_code < 0 {
		// There was an unrecoverable error, leaving the Reader's state
		// undefined. Clear out everything but the buffers.
		*r = Reader{
			buf:              r.buf,
			block_type_trees: r.block_type_trees,
			literal_hgroup: huffmanTreeGroup{
				htrees: r.literal_hgroup.htrees,
				codes:  r.literal_hgroup.codes,
			},
			distance_hgroup: huffmanTreeGroup{
				htrees: r.distance_hgroup.htrees,
				codes:  r.distance_hgroup.codes,
			},
			insert_copy_hgroup: huffmanTreeGroup{
				htrees: r.insert_copy_hgroup.htrees,
				codes:  r.insert_copy_hgroup.codes,
			},
		}
	}

	decoderStateInit(r)
	r.src = src
	r.options = options
	if r.buf == nil {
		r.buf = make([]byte, readBufSize)
	}
	r.in = r.buf[:0]
	r.pendingErr = nil
	r.trailing = nil
	r.streamEnded = false
	return nil
}

// fill reads the next chunk from the underlying reader into r.buf. It never
// loses an error that is returned together with data: a non-nil error
// returned alongside bytes is stashed in r.pendingErr and surfaced after the
// data has been consumed, instead of being discarded.
func (r *Reader) fill() {
	m, readErr := r.src.Read(r.buf)
	r.in = r.buf[:m]
	if m > 0 && readErr != nil {
		r.pendingErr = readErr
		return
	}
	r.pendingErr = readErr
}

func (r *Reader) Read(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	if r.streamEnded {
		return 0, io.EOF
	}

	if r.error_code < 0 {
		return 0, decodeError(decoderGetErrorCode(r))
	}

	if !decoderHasMoreOutput(r) && len(r.in) == 0 {
		if r.pendingErr != nil {
			readErr := r.pendingErr
			r.pendingErr = nil
			if readErr == io.EOF && r.state != stateDone {
				readErr = io.ErrUnexpectedEOF
			}
			return 0, readErr
		}
		r.fill()
		if len(r.in) == 0 {
			readErr := r.pendingErr
			r.pendingErr = nil
			if readErr == nil {
				// The underlying reader returned no data and no error; proxy
				// its behavior rather than spinning on it.
				return 0, nil
			}
			if readErr == io.EOF && r.state != stateDone {
				readErr = io.ErrUnexpectedEOF
			}
			return 0, readErr
		}
	}

	for {
		var written uint
		in_len := uint(len(r.in))
		out_len := uint(len(p))
		in_remaining := in_len
		out_remaining := out_len
		result := decoderDecompressStream(r, &in_remaining, &r.in, &out_remaining, &p)
		written = out_len - out_remaining
		n = int(written)

		switch result {
		case decoderResultSuccess:
			if r.options.SingleStream {
				// Exactly one stream ends here. finishStream hands any bytes
				// that followed the stream (pulled in by readahead) to the
				// trailing reader.
				tailLen := len(r.in)
				if !r.finishStream() {
					return n, errInvalidState
				}
				// If the underlying reader reported a real error together
				// with the data and no trailing byte buffered it, there is
				// nowhere for the error to surface but here: deliver it with
				// the final data instead of dropping it.
				if tailLen == 0 && r.trailing.err != nil && r.trailing.err != io.EOF {
					return n, r.trailing.err
				}
				if n == 0 {
					return 0, io.EOF
				}
				return n, nil
			}
			if len(r.in) > 0 {
				return n, errExcessiveInput
			}
			return n, nil
		case decoderResultError:
			return n, decodeError(decoderGetErrorCode(r))
		case decoderResultNeedsMoreOutput:
			if n == 0 {
				return 0, io.ErrShortBuffer
			}
			return n, nil
		case decoderNeedsMoreInput:
		}

		if len(r.in) != 0 {
			return 0, errInvalidState
		}

		// Calling r.src.Read may block. Don't block if we have data to return.
		if n > 0 {
			return n, nil
		}

		// Top off the buffer, unless a stashed error is already waiting.
		if r.pendingErr != nil {
			readErr := r.pendingErr
			r.pendingErr = nil
			if readErr == io.EOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, readErr
		}
		r.fill()
		if len(r.in) == 0 {
			readErr := r.pendingErr
			r.pendingErr = nil
			// Not enough data to complete decoding.
			if readErr == nil || readErr == io.EOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, readErr
		}
	}
}

// finishStream is called in single-stream mode when the decoder reports that
// the one Brotli stream has fully ended. It snapshots the bytes that remain
// in the internal readahead buffer and marks the stream as ended. It returns
// false if the decoder state is inconsistent.
func (r *Reader) finishStream() bool {
	if r.state != stateDone {
		return false
	}
	// A non-empty internal transaction buffer here would mean some tail bytes
	// had been absorbed by the decoder's byte-at-a-time fallback; the state
	// machine guarantees the transaction is complete at stream end.
	if r.buffer_length != 0 {
		return false
	}

	tail := append([]byte(nil), r.in...)

	// The tail is an independent copy, so the reusable read buffer is not
	// affected by later Reads or a Reset.
	r.trailing = &trailingReader{
		buf: tail,
		src: r.src,
		err: r.pendingErr,
	}
	r.pendingErr = nil
	r.in = nil
	r.streamEnded = true
	return true
}

// TrailingData returns a Reader over the bytes following the decoded Brotli
// stream, when the Reader was created with SingleStream enabled.
//
// The returned Reader first yields any bytes that the Reader already pulled
// from the underlying source while decoding (its internal readahead buffer),
// then continues reading directly from the underlying Reader. Callers do not
// need to track how far the underlying Reader has advanced.
//
// TrailingData must be called after the stream has ended, i.e. after Read has
// returned io.EOF. It returns an error if the stream has not ended yet or if
// the Reader is not in single-stream mode. Repeated calls return the same
// Reader. ResetReader starts a new decoding session: the options supplied to
// it take effect immediately, and TrailingData again reports
// ErrTrailingDataNotReady (or ErrTrailingDataUnsupported, in strict mode)
// until the new stream ends. Readers returned earlier remain usable.
func (r *Reader) TrailingData() (io.Reader, error) {
	if !r.options.SingleStream {
		return nil, errTrailingNotSingleStream
	}
	if !r.streamEnded || r.trailing == nil {
		return nil, errTrailingNotReady
	}
	return r.trailing, nil
}

// trailingReader exposes bytes left over after a Brotli stream. Buffered
// bytes are served first, so callers never have to guess how many bytes the
// decoder already consumed from the underlying reader.
type trailingReader struct {
	buf []byte
	src io.Reader
	err error
}

func (t *trailingReader) Read(p []byte) (int, error) {
	if len(t.buf) > 0 {
		n := copy(p, t.buf)
		t.buf = t.buf[n:]
		if n > 0 {
			return n, nil
		}
	}
	if t.err != nil {
		err := t.err
		t.err = nil
		return 0, err
	}
	return t.src.Read(p)
}
