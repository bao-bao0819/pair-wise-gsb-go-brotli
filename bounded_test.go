package brotli

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"
)

// checkBoundRoundTrip encodes data with a BoundedWriter (applying the given
// write/flush pattern), verifies that the output size is within
// MaxEncodedSize, and decodes the result back with the regular Reader.
func checkBoundRoundTrip(t *testing.T, data []byte, options WriterOptions, maxFlushes int64, write func(w *BoundedWriter, data []byte) (flushes int64)) {
	t.Helper()

	bound, err := MaxEncodedSize(uint64(len(data)), options, maxFlushes)
	if err != nil {
		t.Fatalf("MaxEncodedSize(%d, %+v, %d): %v", len(data), options, maxFlushes, err)
	}

	var buf bytes.Buffer
	w := NewBoundedWriter(&buf, options)
	flushes := write(w, data)
	if flushes > maxFlushes {
		t.Fatalf("test bug: performed %d flushes, maxFlushes is %d", flushes, maxFlushes)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := uint64(buf.Len()); got > bound {
		t.Fatalf("encoded size %d exceeds MaxEncodedSize bound %d (len=%d, options=%+v, flushes=%d)",
			got, bound, len(data), options, flushes)
	}

	decoded, err := io.ReadAll(NewReader(&buf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(decoded), len(data))
	}
}

// writeAll writes data in one go and never flushes.
func writeAll(w *BoundedWriter, data []byte) int64 {
	if _, err := w.Write(data); err != nil {
		panic(err)
	}
	return 0
}

func TestMaxEncodedSizeZeroLength(t *testing.T) {
	for _, lgwin := range []int{0, 10, 16, 22, 24} {
		bound, err := MaxEncodedSize(0, WriterOptions{Quality: 5, LGWin: lgwin}, 0)
		if err != nil {
			t.Fatalf("lgwin %d: %v", lgwin, err)
		}
		if bound == 0 {
			t.Fatalf("lgwin %d: bound for empty input must cover header and trailer", lgwin)
		}
		checkBoundRoundTrip(t, nil, WriterOptions{Quality: 5, LGWin: lgwin}, 0, writeAll)
	}
}

func TestMaxEncodedSizeErrors(t *testing.T) {
	cases := []struct {
		name       string
		srcLen     uint64
		options    WriterOptions
		maxFlushes int64
	}{
		{"negative quality", 100, WriterOptions{Quality: -1}, 0},
		{"quality too high", 100, WriterOptions{Quality: 12}, 0},
		{"lgwin too small", 100, WriterOptions{Quality: 5, LGWin: 9}, 0},
		{"lgwin too large", 100, WriterOptions{Quality: 5, LGWin: 25}, 0},
		{"unknown flushes", 100, WriterOptions{Quality: 5}, -1},
		{"overflowing length", math.MaxUint64 / 8, WriterOptions{Quality: 5}, 0},
		{"overflowing flushes", 100, WriterOptions{Quality: 5}, math.MaxInt64},
	}
	for _, c := range cases {
		if _, err := MaxEncodedSize(c.srcLen, c.options, c.maxFlushes); err == nil {
			t.Errorf("%s: expected error, got none", c.name)
		}
	}
}

func TestBoundedWriterRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	sizes := []int{1, 2, 3, 17, 100, 1000, 65536, 1 << 17, 1<<20 - 1}
	optionSets := []WriterOptions{
		{Quality: 0},
		{Quality: 1, LGWin: 10},
		{Quality: 5, LGWin: 16},
		{Quality: 9, LGWin: 24},
		{Quality: 11},
	}
	for _, options := range optionSets {
		for _, size := range sizes {
			data := make([]byte, size)
			rng.Read(data)
			checkBoundRoundTrip(t, data, options, 0, writeAll)
		}
	}
}

func TestBoundedWriterCompressible(t *testing.T) {
	// Highly compressible data exercises the compressed-block path.
	rng := rand.New(rand.NewSource(7))
	words := [][]byte{[]byte("hello "), []byte("world "), []byte("brotli "), []byte("bounded ")}
	var data []byte
	for len(data) < 3<<20 {
		data = append(data, words[rng.Intn(len(words))]...)
	}
	for _, quality := range []int{0, 4, 9, 11} {
		options := WriterOptions{Quality: quality}
		checkBoundRoundTrip(t, data, options, 0, writeAll)
	}
}

func TestBoundedWriterBlockBoundaries(t *testing.T) {
	// Enumerate sizes around meta-block length field boundaries (2^16, 2^20
	// payload bytes per chunk) and around the chunking boundary itself.
	rng := rand.New(rand.NewSource(99))
	sizes := []int{
		0, 1,
		1<<16 - 1, 1 << 16, 1<<16 + 1,
		1<<20 - 1, 1 << 20, 1<<20 + 1,
		2<<20 - 1, 2 << 20, 2<<20 + 1,
		3 << 20,
	}
	for _, size := range sizes {
		data := make([]byte, size)
		rng.Read(data)
		for _, quality := range []int{1, 6} {
			options := WriterOptions{Quality: quality}
			checkBoundRoundTrip(t, data, options, 0, writeAll)
		}
	}
}

func TestBoundedWriterShortData(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	for size := 0; size <= 300; size++ {
		data := make([]byte, size)
		rng.Read(data)
		options := WriterOptions{Quality: 2, LGWin: 10 + size%15}
		checkBoundRoundTrip(t, data, options, 0, writeAll)
	}
}

func TestBoundedWriterFrequentFlush(t *testing.T) {
	rng := rand.New(rand.NewSource(31337))

	// Flush after every tiny write.
	const pieces = 500
	const pieceLen = 7
	data := make([]byte, pieces*pieceLen)
	rng.Read(data)
	checkBoundRoundTrip(t, data, WriterOptions{Quality: 4}, pieces,
		func(w *BoundedWriter, data []byte) int64 {
			var flushes int64
			for pos := 0; pos < len(data); pos += pieceLen {
				if _, err := w.Write(data[pos : pos+pieceLen]); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				flushes++
			}
			return flushes
		})

	// Flushes with no pending data must emit nothing and not count against
	// the bound; interleave them with real flushes.
	data2 := make([]byte, 100000)
	rng.Read(data2)
	checkBoundRoundTrip(t, data2, WriterOptions{Quality: 9}, 10,
		func(w *BoundedWriter, data []byte) int64 {
			var flushes int64
			step := len(data) / 10
			for pos := 0; pos < len(data); pos += step {
				if _, err := w.Write(data[pos : pos+step]); err != nil {
					t.Fatal(err)
				}
				// Several empty flushes in a row.
				for i := 0; i < 3; i++ {
					if err := w.Flush(); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				flushes++
			}
			return flushes
		})
}

func TestBoundedWriterFlushAcrossChunkBoundary(t *testing.T) {
	// Flush points landing exactly on and around the chunking boundary.
	rng := rand.New(rand.NewSource(555))
	data := make([]byte, 2<<20+3)
	rng.Read(data)
	flushPoints := []int{1<<20 - 1, 1 << 20, 1<<20 + 1}
	checkBoundRoundTrip(t, data, WriterOptions{Quality: 1}, int64(len(flushPoints)),
		func(w *BoundedWriter, data []byte) int64 {
			var flushes int64
			prev := 0
			for _, fp := range flushPoints {
				if _, err := w.Write(data[prev:fp]); err != nil {
					t.Fatal(err)
				}
				if err := w.Flush(); err != nil {
					t.Fatal(err)
				}
				flushes++
				prev = fp
			}
			if _, err := w.Write(data[prev:]); err != nil {
				t.Fatal(err)
			}
			return flushes
		})
}

func TestBoundedWriterEmptyFlushOnly(t *testing.T) {
	// Flushing an empty stream, then closing it, must still produce a valid
	// empty stream within the bound.
	var buf bytes.Buffer
	options := WriterOptions{Quality: 3}
	w := NewBoundedWriter(&buf, options)
	for i := 0; i < 5; i++ {
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bound, err := MaxEncodedSize(0, options, 5)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(buf.Len()) > bound {
		t.Fatalf("encoded size %d exceeds bound %d", buf.Len(), bound)
	}
	decoded, err := io.ReadAll(NewReader(&buf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("expected empty output, got %d bytes", len(decoded))
	}
}

func TestBoundedWriterManySmallWrites(t *testing.T) {
	// Many small writes without flush: data must still be chunked correctly.
	rng := rand.New(rand.NewSource(2718))
	var data []byte
	var buf bytes.Buffer
	options := WriterOptions{Quality: 6}
	w := NewBoundedWriter(&buf, options)
	for len(data) < 1<<20+12345 {
		chunk := make([]byte, 1+rng.Intn(1000))
		rng.Read(chunk)
		data = append(data, chunk...)
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	bound, err := MaxEncodedSize(uint64(len(data)), options, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(buf.Len()) > bound {
		t.Fatalf("encoded size %d exceeds bound %d", buf.Len(), bound)
	}
	decoded, err := io.ReadAll(NewReader(&buf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatal("round trip mismatch")
	}
}
