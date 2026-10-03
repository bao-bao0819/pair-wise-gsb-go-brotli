package brotli

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// chunkedReader returns its data in fixed-size chunks. When errAt is >= 0, the
// given error is delivered together with the first chunk whose end reaches
// errAt, emulating Readers that report an error on the same call as data.
type chunkedReader struct {
	data    []byte
	pos     int
	size    int
	errAt   int
	err     error
	errSent bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := c.size
	if n > len(p) {
		n = len(p)
	}
	if n > len(c.data)-c.pos {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	var err error
	if c.errAt >= 0 && !c.errSent && c.pos >= c.errAt {
		err = c.err
		c.errSent = true
	}
	return n, err
}

// readAll reads r into a fresh buffer of the given size.
func readAll(t *testing.T, r io.Reader, size int) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, size)
	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)
		if err != nil {
			if err != io.EOF {
				t.Fatalf("unexpected error: %v", err)
			}
			return out
		}
	}
}

func singleStreamCases() [][]byte {
	return [][]byte{
		nil,
		[]byte("A"),
		bytes.Repeat([]byte("hello world!"), 1),
		bytes.Repeat([]byte("hello world!"), 50),
		bytes.Repeat([]byte("<html><body>hello</body></html>"), 40),
	}
}

// TestSingleStreamTrailingData feeds a Brotli stream followed by arbitrary
// binary data through every short chunk boundary and output-buffer size, and
// checks that both the decoded data and the exact trailing bytes are recovered.
func TestSingleStreamTrailingData(t *testing.T) {
	encodedTail, err := Encode([]byte("this looks like another brotli stream"), WriterOptions{Quality: 5})
	if err != nil {
		t.Fatal(err)
	}
	tails := [][]byte{
		nil,
		{0},
		bytes.Repeat([]byte{0xDE, 0xAD, 0xBE, 0xEF}, 1),
		bytes.Repeat([]byte("TAIL-DATA-1234567890"), 20),
		encodedTail,
	}

	for _, quality := range []int{0, 4, 9, 11} {
		for ci, content := range singleStreamCases() {
			encoded, err := Encode(content, WriterOptions{Quality: quality})
			if err != nil {
				t.Fatal(err)
			}
			for ti, tail := range tails {
				data := append(append([]byte{}, encoded...), tail...)
				for chunk := 1; chunk <= len(data)+1; chunk++ {
					for outSize := 1; outSize <= 7; outSize += 2 {
						r := NewSingleStreamReader(&chunkedReader{data: data, size: chunk})
						got := readAll(t, r, outSize)
						if !bytes.Equal(got, content) {
							t.Fatalf("quality=%d content=%d tail=%d chunk=%d out=%d: decoded %d bytes, want %d",
								quality, ci, ti, chunk, outSize, len(got), len(content))
						}

						tr, err := r.TrailingReader()
						if err != nil {
							t.Fatalf("quality=%d content=%d tail=%d chunk=%d out=%d: TrailingReader: %v",
								quality, ci, ti, chunk, outSize, err)
						}
						gotTail := readAll(t, tr, outSize)
						if !bytes.Equal(gotTail, tail) {
							t.Fatalf("quality=%d content=%d tail=%d chunk=%d out=%d: tail %x, want %x",
								quality, ci, ti, chunk, outSize, gotTail, tail)
						}
					}
				}
			}
		}
	}
}

// TestSingleStreamDataAndError checks that an error returned by the underlying
// Reader together with the final bytes is neither dropped nor allowed to lose
// the trailing data.
func TestSingleStreamDataAndError(t *testing.T) {
	content := bytes.Repeat([]byte("hello world!"), 30)
	encoded, _ := Encode(content, WriterOptions{Quality: 5})
	tail := []byte("bytes after the stream")
	data := append(append([]byte{}, encoded...), tail...)

	for chunk := 1; chunk <= len(data); chunk++ {
		myErr := errors.New("underlying read failure")
		src := &chunkedReader{data: data, size: chunk, errAt: len(data), err: myErr}
		r := NewSingleStreamReader(src)

		var decoded []byte
		sawErr := false
		buf := make([]byte, 13)
	loop:
		for {
			n, err := r.Read(buf)
			decoded = append(decoded, buf[:n]...)
			switch err {
			case nil:
			case io.EOF:
				break loop
			case myErr:
				sawErr = true
				break loop
			default:
				t.Fatalf("chunk=%d: unexpected error %v", chunk, err)
			}
		}
		if !bytes.Equal(decoded, content) {
			t.Fatalf("chunk=%d: decoded %d bytes, want %d", chunk, len(decoded), len(content))
		}

		tr, err := r.TrailingReader()
		if err != nil {
			t.Fatalf("chunk=%d: TrailingReader: %v", chunk, err)
		}
		var gotTail []byte
		var tailErr error
		for {
			n, err := tr.Read(buf)
			gotTail = append(gotTail, buf[:n]...)
			if err != nil {
				tailErr = err
				break
			}
		}
		if !bytes.Equal(gotTail, tail) {
			t.Fatalf("chunk=%d: tail %q, want %q", chunk, gotTail, tail)
		}
		switch {
		case sawErr && tailErr == io.EOF:
			// Error was already read ahead while decoding the stream.
		case !sawErr && tailErr == myErr:
			// Error surfaced when reading the tail from the source.
		default:
			t.Fatalf("chunk=%d: sawErr=%v tailErr=%v", chunk, sawErr, tailErr)
		}
	}
}

// TestSingleStreamLargeTail uses trailing data bigger than the decoder's
// internal read-ahead buffer, exercising the hand-off to the original Reader.
func TestSingleStreamLargeTail(t *testing.T) {
	content := []byte("small compressed payload")
	encoded, _ := Encode(content, WriterOptions{Quality: 5})
	tail := make([]byte, readBufSize*3+123)
	for i := range tail {
		tail[i] = byte(i*31 + 7)
	}
	data := append(append([]byte{}, encoded...), tail...)

	for _, chunk := range []int{1, 64, 4096, len(data)} {
		r := NewSingleStreamReader(&chunkedReader{data: data, size: chunk})
		if got := readAll(t, r, 4096); !bytes.Equal(got, content) {
			t.Fatalf("chunk=%d: decoded content mismatch (%d bytes)", chunk, len(got))
		}
		tr, err := r.TrailingReader()
		if err != nil {
			t.Fatalf("chunk=%d: TrailingReader: %v", chunk, err)
		}
		gotTail := readAll(t, tr, 5000)
		if !bytes.Equal(gotTail, tail) {
			t.Fatalf("chunk=%d: tail mismatch (%d bytes, want %d)", chunk, len(gotTail), len(tail))
		}
	}
}

// TestSingleStreamErrorsAndOrdering checks truncated/corrupt streams, strict
// mode, and the required call ordering of Read and TrailingReader.
func TestSingleStreamErrorsAndOrdering(t *testing.T) {
	content := bytes.Repeat([]byte("hello world!"), 100)
	encoded, _ := Encode(content, WriterOptions{Quality: 5})
	tail := []byte("the trailing bytes")
	data := append(append([]byte{}, encoded...), tail...)

	// TrailingReader is rejected before decoding finishes.
	r := NewSingleStreamReader(bytes.NewReader(data))
	if _, err := r.TrailingReader(); !errors.Is(err, ErrStreamNotFinished) {
		t.Fatalf("early TrailingReader: got %v, want ErrStreamNotFinished", err)
	}

	// A zero-length Read must not finish the stream.
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("zero-length Read: %d, %v", n, err)
	}
	if _, err := r.TrailingReader(); !errors.Is(err, ErrStreamNotFinished) {
		t.Fatalf("TrailingReader after zero-length Read: got %v", err)
	}

	// A truncated stream is an error; the tail is not available.
	trunc := NewSingleStreamReader(bytes.NewReader(encoded[:len(encoded)-1]))
	if _, err := io.ReadAll(trunc); err == nil {
		t.Fatalf("truncated stream: expected error")
	}
	if _, err := trunc.TrailingReader(); !errors.Is(err, ErrTrailingDataUnavailable) {
		t.Fatalf("TrailingReader after truncation: got %v", err)
	}

	// A corrupt stream is an error.
	corrupt := append([]byte{}, encoded...)
	corrupt[len(corrupt)/2] ^= 0xFF
	bad := NewSingleStreamReader(bytes.NewReader(append(corrupt, tail...)))
	if _, err := io.ReadAll(bad); err == nil {
		t.Fatalf("corrupt stream: expected error")
	}

	// Strict mode is unchanged: trailing data is still "excessive input".
	strict := NewReader(bytes.NewReader(data))
	if _, err := io.ReadAll(strict); !errors.Is(err, errExcessiveInput) {
		t.Fatalf("strict mode: got %v, want excessive input", err)
	}

	// After a successful decode the tail is delivered once, and reads keep
	// ending with io.EOF.
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("decode: %v", err)
	}
	tr, err := r.TrailingReader()
	if err != nil {
		t.Fatalf("TrailingReader: %v", err)
	}
	// A zero-length Read from the trailing reader is a no-op.
	if n, err := tr.Read(nil); n != 0 || err != nil {
		t.Fatalf("trailing zero-length Read: %d, %v", n, err)
	}
	gotTail, err := io.ReadAll(tr)
	if err != nil || !bytes.Equal(gotTail, tail) {
		t.Fatalf("tail: %q, %v", gotTail, err)
	}
	if _, err := r.TrailingReader(); !errors.Is(err, ErrTrailingDataUnavailable) {
		t.Fatalf("repeated TrailingReader: got %v", err)
	}
	if _, err := r.Read(make([]byte, 4)); err != io.EOF {
		t.Fatalf("Read after finish: got %v, want EOF", err)
	}

	// Reset keeps single-stream mode and makes a fresh tail available.
	if err := r.Reset(bytes.NewReader(data)); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("decode after Reset: %v", err)
	}
	tr, _ = r.TrailingReader()
	gotTail, _ = io.ReadAll(tr)
	if !bytes.Equal(gotTail, tail) {
		t.Fatalf("tail after Reset: %q", gotTail)
	}

	// Reset after a stream has finished invalidates a second request.
	if err := r.Reset(bytes.NewReader(data)); err != nil {
		t.Fatalf("second Reset: %v", err)
	}
	if _, err := r.TrailingReader(); !errors.Is(err, ErrStreamNotFinished) {
		t.Fatalf("TrailingReader right after Reset: got %v", err)
	}

	// Trailing data that happens to be another Brotli stream is not decoded.
	double := append(append([]byte{}, encoded...), encoded...)
	r = NewSingleStreamReader(bytes.NewReader(double))
	got, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("double stream decode: %d bytes, %v", len(got), err)
	}
	tr, _ = r.TrailingReader()
	gotTail, _ = io.ReadAll(tr)
	if !bytes.Equal(gotTail, encoded) {
		t.Fatalf("double stream tail: got %d bytes, want %d", len(gotTail), len(encoded))
	}
}
