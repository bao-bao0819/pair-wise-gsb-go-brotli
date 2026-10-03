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

// readBufSize is a "good" buffer size that avoids excessive round-trips
// between C and Go but doesn't waste too much memory on buffering.
// It is arbitrarily chosen to be equal to the constant used in io.Copy.
const readBufSize = 32 * 1024

// NewReader creates a new Reader reading the given reader.
func NewReader(src io.Reader) *Reader {
	r := new(Reader)
	r.Reset(src)
	return r
}

// Reset discards the Reader's state and makes it equivalent to the result of
// its original state from NewReader, but reading from src instead.
// This permits reusing a Reader rather than allocating a new one.
// Error is always nil
func (r *Reader) Reset(src io.Reader) error {
	singleStream := r.singleStream
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
	if r.buf == nil {
		r.buf = make([]byte, readBufSize)
	}
	r.in = r.buf[:0]
	r.singleStream = singleStream
	r.streamDone = false
	r.decodeFailed = false
	r.tailBytes = nil
	r.pendingReadErr = nil
	r.trailingTaken = false
	return nil
}

func (r *Reader) fail(err error) (int, error) {
	r.decodeFailed = true
	return 0, err
}

// readFromSrc refills the internal buffer from src. An error returned together
// with data (for example io.EOF on the final chunk) is kept instead of being
// dropped: the data is processed first, and the error is returned afterwards.
func (r *Reader) readFromSrc() {
	m, readErr := r.src.Read(r.buf)
	r.in = r.buf[:m]
	r.pendingReadErr = readErr
}

func (r *Reader) Read(p []byte) (n int, err error) {
	// In single-stream mode the end of the current Brotli stream is terminal.
	if r.singleStream && r.streamDone {
		if r.pendingReadErr != nil {
			err = r.pendingReadErr
			r.pendingReadErr = nil
			return 0, err
		}
		return 0, io.EOF
	}

	if !decoderHasMoreOutput(r) && len(r.in) == 0 {
		if r.pendingReadErr != nil {
			readErr := r.pendingReadErr
			r.pendingReadErr = nil
			if readErr == io.EOF && r.state != stateDone {
				readErr = io.ErrUnexpectedEOF
			}
			if r.singleStream {
				return r.fail(readErr)
			}
			return 0, readErr
		}
		r.readFromSrc()
		m := len(r.in)
		if m == 0 {
			readErr := r.pendingReadErr
			r.pendingReadErr = nil
			if readErr == io.EOF && r.state != stateDone {
				readErr = io.ErrUnexpectedEOF
			}
			// If readErr is `nil`, we just proxy underlying stream behavior.
			if r.singleStream {
				return r.fail(readErr)
			}
			return 0, readErr
		}
	}

	if len(p) == 0 {
		return 0, nil
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
			if r.singleStream {
				r.finishSingleStream()
				if n == 0 {
					// The stream is fully decoded and nothing was produced for
					// this call. End it with io.EOF; an error returned together
					// with the final bytes is left for the TrailingReader,
					// because it concerns bytes after the Brotli stream.
					if r.pendingReadErr == io.EOF {
						r.pendingReadErr = nil
					}
					if r.pendingReadErr != nil {
						err = r.pendingReadErr
						r.pendingReadErr = nil
						return n, err
					}
					return n, io.EOF
				}
				return n, nil
			}
			if len(r.in) > 0 {
				return n, errExcessiveInput
			}
			if r.pendingReadErr != nil {
				err = r.pendingReadErr
				r.pendingReadErr = nil
				return n, err
			}
			return n, nil
		case decoderResultError:
			if r.singleStream {
				r.decodeFailed = true
			}
			return n, decodeError(decoderGetErrorCode(r))
		case decoderResultNeedsMoreOutput:
			if n == 0 {
				if r.singleStream {
					r.decodeFailed = true
				}
				return 0, io.ErrShortBuffer
			}
			return n, nil
		case decoderNeedsMoreInput:
		}

		if len(r.in) != 0 {
			if r.singleStream {
				r.decodeFailed = true
			}
			return 0, errInvalidState
		}

		// Calling r.src.Read may block. Don't block if we have data to return.
		if n > 0 {
			return n, nil
		}

		// Surface an error that arrived together with the preceding chunk
		// instead of issuing another read that could hide it.
		if r.pendingReadErr != nil {
			readErr := r.pendingReadErr
			r.pendingReadErr = nil
			if readErr == io.EOF {
				if r.singleStream {
					return r.fail(io.ErrUnexpectedEOF)
				}
				return 0, io.ErrUnexpectedEOF
			}
			if r.singleStream {
				return r.fail(readErr)
			}
			return 0, readErr
		}

		// Top off the buffer.
		r.readFromSrc()
		encN := len(r.in)
		if encN == 0 {
			// Not enough data to complete decoding.
			readErr := r.pendingReadErr
			r.pendingReadErr = nil
			if readErr == io.EOF {
				if r.singleStream {
					return r.fail(io.ErrUnexpectedEOF)
				}
				return 0, io.ErrUnexpectedEOF
			}
			if r.singleStream {
				return r.fail(readErr)
			}
			return 0, readErr
		}
	}
}

// finishSingleStream marks the decoded stream as done and preserves the bytes
// that follow it. It accounts both for the decoder's byte-aligned bit reader
// (including the padding bits of the last block) and for its internal
// byte-by-byte transaction buffer, as well as the 32 KiB read-ahead.
func (r *Reader) finishSingleStream() {
	br := &r.br

	if r.buffer_length != 0 {
		// The final read transaction was served from the internal byte buffer.
		// Rewind any whole bytes still held in the bit reader accumulator, then
		// collect the internal-buffer remainder followed by the rest of the
		// current read-ahead chunk, in the order they were taken from src.
		bitReaderUnload(br)
		r.tailBytes = append(r.tailBytes[:0], br.input[br.byte_pos:br.input_len]...)
		r.tailBytes = append(r.tailBytes, r.in...)
	} else {
		// Decoder already unloaded the bit reader and advanced r.in to the
		// first byte after the stream.
		r.tailBytes = append(r.tailBytes[:0], r.in...)
	}
	r.streamDone = true
}
