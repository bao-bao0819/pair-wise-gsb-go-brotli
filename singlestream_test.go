package brotli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// fixedChunkReader delivers at most n bytes per Read and counts calls.
type fixedChunkReader struct {
	data     []byte
	n        int
	reads    int
	finalErr error // returned once data is exhausted (defaults to io.EOF)
}

func (c *fixedChunkReader) Read(p []byte) (int, error) {
	c.reads++
	if len(c.data) == 0 {
		err := c.finalErr
		if err == nil {
			err = io.EOF
		}
		return 0, err
	}
	n := c.n
	if n <= 0 {
		n = 1
	}
	if n > len(c.data) {
		n = len(c.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// dataAndErrorReader returns one chunk, optionally paired with err.
type dataAndErrorReader struct {
	chunk []byte
	err   error
	done  bool
}

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.chunk)
	if n == len(r.chunk) {
		return n, r.err
	}
	// Caller's buffer was smaller than the chunk; keep the remainder.
	r.chunk = r.chunk[n:]
	r.done = false
	return n, nil
}

func brotliBytes(t *testing.T, content []byte, quality int) []byte {
	t.Helper()
	encoded, err := Encode(content, WriterOptions{Quality: quality})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return encoded
}

// readOneByteAtATime decodes r using 1-byte destination buffers and returns
// the output. It mirrors how a minimal caller would drive the Reader.
func readOneByteAtATime(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var out bytes.Buffer
	var one [1]byte
	for {
		n, err := r.Read(one[:])
		out.Write(one[:n])
		if err == io.EOF {
			return out.Bytes()
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
}

func trailingBytes(t *testing.T, tr io.Reader) []byte {
	t.Helper()
	b, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("trailing ReadAll: %v", err)
	}
	return b
}

// TestSingleStreamAllChunkBoundaries checks that, for compressed data
// followed by an arbitrary binary trailer, the full payload is recovered at
// every possible short read boundary and every quality level.
func TestSingleStreamAllChunkBoundaries(t *testing.T) {
	trailer := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x00, 0x11, 0x22, 0x33}

	for _, quality := range []int{0, 2, 5, 9, 11} {
		for _, content := range [][]byte{
			nil,
			[]byte("A"),
			bytes.Repeat([]byte("hello world!"), 50),
		} {
			encoded := brotliBytes(t, content, quality)
			payload := append(append([]byte{}, encoded...), trailer...)

			for chunk := 1; chunk <= len(payload); chunk++ {
				name := fmt.Sprintf("q%d/len%d/chunk%d", quality, len(content), chunk)
				t.Run(name, func(t *testing.T) {
					src := &fixedChunkReader{
						data: append([]byte{}, payload...),
						n:    chunk,
					}
					r := NewReaderOptions(src, ReaderOptions{SingleStream: true})

					out := readOneByteAtATime(t, r)
					if !bytes.Equal(out, content) {
						t.Fatalf("decoded %d bytes, want %d", len(out), len(content))
					}

					tr, err := r.TrailingData()
					if err != nil {
						t.Fatalf("TrailingData: %v", err)
					}
					got := trailingBytes(t, tr)
					if !bytes.Equal(got, trailer) {
						t.Fatalf("trailer = %x, want %x", got, trailer)
					}

					// Reads after the stream ended return io.EOF and do not
					// consume anything more from the underlying source.
					reads := src.reads
					var buf [8]byte
					if n, err := r.Read(buf[:]); n != 0 || err != io.EOF {
						t.Fatalf("post-end Read = (%d,%v), want (0,EOF)", n, err)
					}
					if src.reads != reads {
						t.Fatalf("post-end Read advanced the underlying source")
					}
				})
			}
		}
	}
}

// TestSingleStreamLargeReadahead exercises a source that hands over the
// whole concatenation (stream + trailer) in a single big read, including a
// trailer longer than the decoder's internal buffer assumptions.
func TestSingleStreamLargeReadahead(t *testing.T) {
	content := bytes.Repeat([]byte("abcdefghij"), 1000)
	encoded := brotliBytes(t, content, 5)
	trailer := bytes.Repeat([]byte{0x01, 0x02, 0x03, 0x04}, 10000)
	payload := append(append([]byte{}, encoded...), trailer...)

	r := NewReaderOptions(bytes.NewReader(payload), ReaderOptions{SingleStream: true})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(out, content) {
		t.Fatalf("decoded length = %d, want %d", len(out), len(content))
	}

	tr, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData: %v", err)
	}
	got := trailingBytes(t, tr)
	if !bytes.Equal(got, trailer) {
		t.Fatalf("recovered %d trailer bytes, want %d", len(got), len(trailer))
	}
}

// TestSingleStreamNoTrailer checks that a stream ending exactly at EOF is
// valid and yields an empty trailer.
func TestSingleStreamNoTrailer(t *testing.T) {
	content := []byte("ends right at EOF")
	encoded := brotliBytes(t, content, 5)

	r := NewReaderOptions(bytes.NewReader(encoded), ReaderOptions{SingleStream: true})
	out := readOneByteAtATime(t, r)
	if !bytes.Equal(out, content) {
		t.Fatalf("decoded %q, want %q", out, content)
	}
	tr, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData: %v", err)
	}
	if got := trailingBytes(t, tr); len(got) != 0 {
		t.Fatalf("trailer = %x, want empty", got)
	}
}

// TestSingleStreamTrailerLooksLikeStream verifies that a trailer that is
// itself a valid Brotli stream is returned verbatim and never auto-merged
// into the first stream's output.
func TestSingleStreamTrailerLooksLikeStream(t *testing.T) {
	first := []byte("first stream payload")
	second := []byte("second stream payload")
	s1 := brotliBytes(t, first, 5)
	s2 := brotliBytes(t, second, 5)
	payload := append(append([]byte{}, s1...), s2...)

	for chunk := 1; chunk <= len(payload); chunk++ {
		r := NewReaderOptions(
			&fixedChunkReader{data: append([]byte{}, payload...), n: chunk},
			ReaderOptions{SingleStream: true},
		)
		out := readOneByteAtATime(t, r)
		if !bytes.Equal(out, first) {
			t.Fatalf("chunk %d: decoded %q, want only the first stream %q", chunk, out, first)
		}
		tr, err := r.TrailingData()
		if err != nil {
			t.Fatalf("chunk %d: TrailingData: %v", chunk, err)
		}
		leftover := trailingBytes(t, tr)
		if !bytes.Equal(leftover, s2) {
			t.Fatalf("chunk %d: trailer not returned verbatim (got %d bytes, want %d)",
				chunk, len(leftover), len(s2))
		}

		// The second stream decodes only when the caller asks for it.
		r2 := NewReaderOptions(bytes.NewReader(leftover), ReaderOptions{SingleStream: true})
		out2 := readOneByteAtATime(t, r2)
		if !bytes.Equal(out2, second) {
			t.Fatalf("chunk %d: explicit second stream decoded %q, want %q", chunk, out2, second)
		}
	}
}

// TestStrictModeStillRejectsTrailer pins the default behavior: trailers are
// reported as excessive input and strict mode stays the default.
func TestStrictModeStillRejectsTrailer(t *testing.T) {
	content := []byte("strict mode content")
	encoded := brotliBytes(t, content, 5)
	payload := append(append([]byte{}, encoded...), 1, 2, 3)

	t.Run("default NewReader", func(t *testing.T) {
		r := NewReader(bytes.NewReader(payload))
		if _, err := io.ReadAll(r); !errors.Is(err, ErrExcessiveInput) {
			t.Fatalf("ReadAll err = %v, want ErrExcessiveInput", err)
		}
	})

	t.Run("explicit strict options", func(t *testing.T) {
		r := NewReaderOptions(bytes.NewReader(payload), ReaderOptions{})
		if _, err := io.ReadAll(r); !errors.Is(err, ErrExcessiveInput) {
			t.Fatalf("ReadAll err = %v, want ErrExcessiveInput", err)
		}
		if _, err := r.TrailingData(); !errors.Is(err, ErrTrailingDataUnsupported) {
			t.Fatalf("TrailingData err = %v, want ErrTrailingDataUnsupported", err)
		}
	})

	t.Run("clean stream still fine", func(t *testing.T) {
		r := NewReader(bytes.NewReader(encoded))
		out, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(out, content) {
			t.Fatalf("clean stream: out %d bytes, err %v", len(out), err)
		}
	})
}

// TestTrailingDataCallOrder verifies the checkable sequencing: unavailable
// before the end, identical on repeat, and reset by Reset.
func TestTrailingDataCallOrder(t *testing.T) {
	content := bytes.Repeat([]byte("xy"), 100)
	encoded := brotliBytes(t, content, 5)
	payload := append(append([]byte{}, encoded...), 9, 8, 7)

	r := NewReaderOptions(bytes.NewReader(payload), ReaderOptions{SingleStream: true})

	// Not ready before decoding starts.
	if _, err := r.TrailingData(); !errors.Is(err, ErrTrailingDataNotReady) {
		t.Fatalf("TrailingData before Read err = %v, want ErrTrailingDataNotReady", err)
	}

	// Still not ready halfway through the stream.
	var first [4]byte
	if n, err := r.Read(first[:]); err != nil || n != 4 {
		t.Fatalf("first Read = (%d,%v)", n, err)
	}
	if _, err := r.TrailingData(); !errors.Is(err, ErrTrailingDataNotReady) {
		t.Fatalf("TrailingData mid-stream err = %v, want ErrTrailingDataNotReady", err)
	}

	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(append(first[:], out...), content) {
		t.Fatalf("decoded content mismatch")
	}

	tr1, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData after end: %v", err)
	}
	tr2, err := r.TrailingData()
	if err != nil {
		t.Fatalf("repeat TrailingData: %v", err)
	}
	if tr1 != tr2 {
		t.Fatalf("repeat TrailingData returned a different Reader")
	}

	firstTrailer, err := io.ReadAll(tr1)
	if err != nil || !bytes.Equal(firstTrailer, []byte{9, 8, 7}) {
		t.Fatalf("trailer read = %x,%v", firstTrailer, err)
	}

	// ResetReader starts a fresh single stream; TrailingData is not ready
	// until it ends.
	encoded2 := brotliBytes(t, []byte("new stream"), 5)
	if err := r.ResetReader(bytes.NewReader(encoded2), ReaderOptions{SingleStream: true}); err != nil {
		t.Fatalf("ResetReader: %v", err)
	}
	if _, err := r.TrailingData(); !errors.Is(err, ErrTrailingDataNotReady) {
		t.Fatalf("TrailingData after Reset err = %v, want ErrTrailingDataNotReady", err)
	}
	out2 := readOneByteAtATime(t, r)
	if string(out2) != "new stream" {
		t.Fatalf("after Reset decoded %q, want %q", out2, "new stream")
	}
	if _, err := r.TrailingData(); err != nil {
		t.Fatalf("TrailingData after new stream: %v", err)
	}
}

// TestResetReaderSwitchesMode verifies ResetReader applies the new options.
func TestResetReaderSwitchesMode(t *testing.T) {
	content := []byte("mode switching")
	encoded := brotliBytes(t, content, 5)
	withTrailer := append(append([]byte{}, encoded...), 0xAA)

	r := NewReaderOptions(bytes.NewReader(withTrailer), ReaderOptions{})
	if _, err := io.ReadAll(r); !errors.Is(err, ErrExcessiveInput) {
		t.Fatalf("strict: %v", err)
	}

	if err := r.ResetReader(bytes.NewReader(withTrailer), ReaderOptions{SingleStream: true}); err != nil {
		t.Fatalf("ResetReader: %v", err)
	}
	out := readOneByteAtATime(t, r)
	if !bytes.Equal(out, content) {
		t.Fatalf("single mode decoded %q, want %q", out, content)
	}
	tr, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData: %v", err)
	}
	if got := trailingBytes(t, tr); !bytes.Equal(got, []byte{0xAA}) {
		t.Fatalf("trailer = %x", got)
	}

	// Switching back to strict rejects the trailer again.
	if err := r.ResetReader(bytes.NewReader(withTrailer), ReaderOptions{}); err != nil {
		t.Fatalf("ResetReader back: %v", err)
	}
	if _, err := io.ReadAll(r); !errors.Is(err, ErrExcessiveInput) {
		t.Fatalf("strict again: %v", err)
	}
}

// TestSingleStreamTruncatedIsError verifies a truncated stream is still an
// error and leaves TrailingData unavailable.
func TestSingleStreamTruncatedIsError(t *testing.T) {
	content := bytes.Repeat([]byte("truncation test"), 20)
	encoded := brotliBytes(t, content, 5)

	for cut := 0; cut < len(encoded); cut++ {
		truncated := encoded[:cut]
		r := NewReaderOptions(
			&fixedChunkReader{data: append([]byte{}, truncated...), n: 1},
			ReaderOptions{SingleStream: true},
		)
		_, err := io.ReadAll(r)
		if err == nil {
			t.Fatalf("cut %d: expected error for truncated stream", cut)
		}
		if _, terr := r.TrailingData(); !errors.Is(terr, ErrTrailingDataNotReady) {
			t.Fatalf("cut %d: TrailingData err = %v, want ErrTrailingDataNotReady", cut, terr)
		}
	}
}

// TestSingleStreamCorrupt verifies that a corrupt stream is reported as an
// error (never silently swallowed) and that the decoder never scans the
// following bytes looking for another stream: TrailingData is unavailable
// after an error.
func TestSingleStreamCorruptIsError(t *testing.T) {
	content := bytes.Repeat([]byte("corruption!!"), 20)
	encoded := brotliBytes(t, content, 5)
	s2 := brotliBytes(t, []byte("a second valid stream"), 5)

	// Truncating most prefixes leaves the decoder mid-symbol; appending a
	// valid second stream must not cause it to skip ahead and decode that.
	sawError := false
	for cut := 1; cut < len(encoded); cut++ {
		payload := append(append([]byte{}, encoded[:cut]...), s2...)
		r := NewReaderOptions(
			&fixedChunkReader{data: payload, n: 3},
			ReaderOptions{SingleStream: true},
		)
		out, err := io.ReadAll(r)
		if err == nil {
			// A prefix that happens to remain a valid stream may decode, but
			// it can only ever produce data of the first stream's length and
			// must never absorb the appended second stream.
			if len(out) > len(content) || bytes.Contains(out, []byte("second")) {
				t.Fatalf("cut %d: decoder absorbed following stream", cut)
			}
			continue
		}
		sawError = true
		if _, terr := r.TrailingData(); terr == nil {
			t.Fatalf("cut %d: TrailingData available after error", cut)
		}
	}
	if !sawError {
		t.Fatalf("no truncation produced an error; test assumptions changed")
	}

	// Flipping interior bits: either an error (TrailingData unavailable) or
	// output confined to the first stream with the second stream preserved.
	for i := 0; i < len(encoded); i++ {
		damaged := append([]byte{}, encoded...)
		damaged[i] ^= 0xFF
		payload := append(damaged, s2...)
		r := NewReaderOptions(
			&fixedChunkReader{data: payload, n: 3},
			ReaderOptions{SingleStream: true},
		)
		out, err := io.ReadAll(r)
		if err != nil {
			if _, terr := r.TrailingData(); !errors.Is(terr, ErrTrailingDataNotReady) {
				t.Fatalf("byte %d: TrailingData after error = %v, want ErrTrailingDataNotReady", i, terr)
			}
			continue
		}
		// Decoding "succeeded": it must not have swallowed the second stream.
		if len(out) > len(content) || bytes.Contains(out, []byte("second")) {
			t.Fatalf("byte %d: decoder scanned/merged the following stream", i)
		}
		// And the bytes after the first stream must include the untouched
		// second stream verbatim in the trailer.
		tr, terr := r.TrailingData()
		if terr != nil {
			t.Fatalf("byte %d: TrailingData: %v", i, terr)
		}
		got, rerr := io.ReadAll(tr)
		if rerr != nil {
			t.Fatalf("byte %d: trailer read: %v", i, rerr)
		}
		if !bytes.HasSuffix(got, s2) {
			t.Fatalf("byte %d: following stream not preserved verbatim in trailer", i)
		}
	}
}

// TestUnderlyingReadDataAndError ensures an error returned together with the
// final bytes is never dropped: during decoding it surfaces as
// ErrUnexpectedEOF (the stream is incomplete without an EOF), and with a
// complete stream plus trailer it is delivered to the trailer reader.
func TestUnderlyingReadDataAndError(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("mid stream", func(t *testing.T) {
		content := bytes.Repeat([]byte("mid stream"), 30)
		encoded := brotliBytes(t, content, 5)
		// Drop the final byte so the stream is incomplete: the error
		// returned with the data must not be swallowed into a plain EOF.
		incomplete := encoded[:len(encoded)-1]
		r := NewReaderOptions(
			&dataAndErrorReader{chunk: incomplete, err: sentinel},
			ReaderOptions{SingleStream: true},
		)
		_, err := io.ReadAll(r)
		if !errors.Is(err, sentinel) && !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v, want sentinel or ErrUnexpectedEOF", err)
		}
	})

	t.Run("complete stream with trailer", func(t *testing.T) {
		content := []byte("complete")
		encoded := brotliBytes(t, content, 5)
		trailer := []byte{1, 2, 3, 4, 5}
		payload := append(append([]byte{}, encoded...), trailer...)
		r := NewReaderOptions(
			&dataAndErrorReader{chunk: payload, err: sentinel},
			ReaderOptions{SingleStream: true},
		)
		out := readOneByteAtATime(t, r)
		if !bytes.Equal(out, content) {
			t.Fatalf("decoded %q, want %q", out, content)
		}
		tr, err := r.TrailingData()
		if err != nil {
			t.Fatalf("TrailingData: %v", err)
		}
		got, err := io.ReadAll(tr)
		if !errors.Is(err, sentinel) {
			t.Fatalf("trailer err = %v, want sentinel", err)
		}
		if !bytes.Equal(got, trailer) {
			t.Fatalf("trailer = %x, want %x", got, trailer)
		}
	})

	t.Run("strict mode keeps error", func(t *testing.T) {
		content := []byte("strict sentinel")
		encoded := brotliBytes(t, content, 5)
		r := NewReader(&dataAndErrorReader{chunk: encoded, err: sentinel})
		got, err := io.ReadAll(r)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want sentinel", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("decoded %q, want %q", got, content)
		}
	})
}

// TestZeroLengthRead checks the documented zero-length Read semantics at end
// of stream and before it.
func TestZeroLengthRead(t *testing.T) {
	content := []byte("zero length reads")
	encoded := brotliBytes(t, content, 5)
	payload := append(append([]byte{}, encoded...), 7)

	r := NewReaderOptions(bytes.NewReader(payload), ReaderOptions{SingleStream: true})

	// Zero-length Read before the end returns (0, nil) and changes nothing.
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) before end = (%d,%v), want (0,nil)", n, err)
	}

	out := readOneByteAtATime(t, r)
	if !bytes.Equal(out, content) {
		t.Fatalf("decoded %q, want %q", out, content)
	}
	tr, _ := r.TrailingData()
	if got := trailingBytes(t, tr); !bytes.Equal(got, []byte{7}) {
		t.Fatalf("trailer = %x", got)
	}

	// Zero-length Read keeps the standard (0, nil) contract; a real read
	// after the end reports io.EOF.
	if n, err := r.Read(make([]byte, 0)); n != 0 || err != nil {
		t.Fatalf("Read(empty) after end = (%d,%v), want (0,nil)", n, err)
	}
	var one [1]byte
	if n, err := r.Read(one[:]); n != 0 || err != io.EOF {
		t.Fatalf("Read after end = (%d,%v), want (0,EOF)", n, err)
	}
}

// TestSingleStreamTrailerOneByteReads verifies the trailer reader itself
// works with tiny reads across the buffered/unbuffered handoff.
func TestSingleStreamTrailerOneByteReads(t *testing.T) {
	content := []byte("handoff")
	encoded := brotliBytes(t, content, 5)
	trailer := bytes.Repeat([]byte{0x5A}, 40000) // larger than the 32KiB read buffer
	payload := append(append([]byte{}, encoded...), trailer...)

	src := &fixedChunkReader{data: payload, n: 7}
	r := NewReaderOptions(src, ReaderOptions{SingleStream: true})
	out := readOneByteAtATime(t, r)
	if !bytes.Equal(out, content) {
		t.Fatalf("decoded mismatch")
	}
	tr, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData: %v", err)
	}

	var got []byte
	var one [1]byte
	for {
		n, err := tr.Read(one[:])
		if n == 1 {
			got = append(got, one[0])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("trailer Read: %v", err)
		}
	}
	if !bytes.Equal(got, trailer) {
		t.Fatalf("recovered %d trailer bytes, want %d", len(got), len(trailer))
	}
}

// TestSingleStreamLargeOutput verifies streams spanning many output chunks
// and internal ringbuffer flushes with a trailing byte.
func TestSingleStreamLargeOutput(t *testing.T) {
	content := bytes.Repeat([]byte("the quick brown fox jumps over the lazy dog. "), 5000)
	encoded := brotliBytes(t, content, 5)
	payload := append(append([]byte{}, encoded...), 0x42)

	r := NewReaderOptions(
		&fixedChunkReader{data: payload, n: 17},
		ReaderOptions{SingleStream: true},
	)
	out := readOneByteAtATime(t, r)
	if !bytes.Equal(out, content) {
		t.Fatalf("large output mismatch: %d vs %d", len(out), len(content))
	}
	tr, err := r.TrailingData()
	if err != nil {
		t.Fatalf("TrailingData: %v", err)
	}
	if got := trailingBytes(t, tr); !bytes.Equal(got, []byte{0x42}) {
		t.Fatalf("trailer = %x", got)
	}
}
