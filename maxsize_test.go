package brotli

import (
	"bytes"
	"io"
	"math"
	"math/rand"
	"testing"
)

// encodeBounded compresses data with a bounded-mode writer, splitting the
// input into chunks of chunkSize bytes and flushing every flushEvery
// chunks. It returns the compressed output and the number of flushes.
func encodeBounded(t *testing.T, data []byte, quality, lgwin, chunkSize, flushEvery int) ([]byte, int) {
	t.Helper()
	var buf bytes.Buffer
	w := NewWriterOptions(&buf, WriterOptions{Quality: quality, LGWin: lgwin, Bounded: true})
	flushes := 0
	for i := 0; i < len(data); {
		end := i + chunkSize
		if end > len(data) {
			end = len(data)
		}
		if _, err := w.Write(data[i:end]); err != nil {
			t.Fatalf("Write: %v", err)
		}
		i = end
		if flushEvery > 0 && i < len(data) && (i/chunkSize)%flushEvery == 0 {
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			flushes++
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes(), flushes
}

// checkBound verifies that encoded is no longer than the MaxEncodedSize
// bound and that it decodes back to data with the stock decoder.
func checkBound(t *testing.T, data, encoded []byte, options WriterOptions, flushes int) {
	t.Helper()
	bound, err := MaxEncodedSize(uint64(len(data)), options, flushes)
	if err != nil {
		t.Fatalf("MaxEncodedSize(%d, %+v, %d): %v", len(data), options, flushes, err)
	}
	if uint64(len(encoded)) > bound {
		t.Fatalf("MaxEncodedSize(%d, %+v, %d) = %d, but encoded output is %d bytes",
			len(data), options, flushes, bound, len(encoded))
	}
	decoded, err := io.ReadAll(NewReader(bytes.NewReader(encoded)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(decoded), len(data))
	}
}

func TestMaxEncodedSizeRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	qualities := []int{0, 1, 2, 5, 9, 11}
	lgwins := []int{0, 10, 16, 22, 24}
	for iter := 0; iter < 60; iter++ {
		quality := qualities[rng.Intn(len(qualities))]
		lgwin := lgwins[rng.Intn(len(lgwins))]
		size := rng.Intn(1 << 18)
		// Random (incompressible) data is the worst case for expansion.
		data := make([]byte, size)
		rng.Read(data)
		chunkSize := 1 + rng.Intn(4096)
		flushEvery := rng.Intn(8) // 0 means "no flushes"
		options := WriterOptions{Quality: quality, LGWin: lgwin}
		encoded, flushes := encodeBounded(t, data, quality, lgwin, chunkSize, flushEvery)
		checkBound(t, data, encoded, options, flushes)
	}
}

func TestMaxEncodedSizeBlockBoundaries(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	sizes := []int{
		0, 1, 2, 3,
		1023, 1024, 1025, // smallest bounded segment size (LGWin 10)
		minNonForcedBlockPayload - 1, minNonForcedBlockPayload, minNonForcedBlockPayload + 1,
		1<<16 - 1, 1 << 16, 1<<16 + 1,
		1<<17 - 1, 1 << 17, 1<<17 + 1,
		1 << 18,
	}
	qualities := []int{0, 1, 2, 6, 11}
	for _, quality := range qualities {
		for _, lgwin := range []int{10, 22} {
			for _, size := range sizes {
				data := make([]byte, size)
				rng.Read(data)
				options := WriterOptions{Quality: quality, LGWin: lgwin}
				encoded, flushes := encodeBounded(t, data, quality, lgwin, 4096, 3)
				checkBound(t, data, encoded, options, flushes)
			}
		}
	}
}

func TestMaxEncodedSizeShortData(t *testing.T) {
	rng := rand.New(rand.NewSource(13))
	for size := 0; size <= 300; size++ {
		data := make([]byte, size)
		rng.Read(data)
		for _, quality := range []int{0, 1, 2, 11} {
			for _, flushEvery := range []int{0, 1, 7} {
				options := WriterOptions{Quality: quality}
				encoded, flushes := encodeBounded(t, data, quality, 0, 64, flushEvery)
				checkBound(t, data, encoded, options, flushes)
			}
		}
	}
}

func TestMaxEncodedSizeFrequentFlush(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	data := make([]byte, 10000)
	rng.Read(data)
	for _, quality := range []int{0, 1, 5, 11} {
		options := WriterOptions{Quality: quality}
		// Flush after every 100-byte chunk: 99 flushes total.
		encoded, flushes := encodeBounded(t, data, quality, 0, 100, 1)
		if flushes != 99 {
			t.Fatalf("got %d flushes, want 99", flushes)
		}
		checkBound(t, data, encoded, options, flushes)
	}
}

// TestMaxEncodedSizeByteWrites writes one byte at a time, the worst-case
// Write pattern for the quality 0/1 fast paths without bounded mode.
func TestMaxEncodedSizeByteWrites(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	data := make([]byte, 5000)
	rng.Read(data)
	for _, quality := range []int{0, 1, 2, 11} {
		options := WriterOptions{Quality: quality, LGWin: 10}
		encoded, flushes := encodeBounded(t, data, quality, 10, 1, 0)
		checkBound(t, data, encoded, options, flushes)
	}
}

func TestMaxEncodedSizeZero(t *testing.T) {
	for _, quality := range []int{0, 1, 2, 6, 11} {
		options := WriterOptions{Quality: quality}
		bound, err := MaxEncodedSize(0, options, 0)
		if err != nil {
			t.Fatalf("MaxEncodedSize(0, %+v, 0): %v", options, err)
		}
		if bound == 0 {
			t.Fatalf("MaxEncodedSize(0, %+v, 0) = 0, want a positive bound for the empty stream", options)
		}
		var buf bytes.Buffer
		w := NewWriterOptions(&buf, WriterOptions{Quality: quality, Bounded: true})
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if uint64(buf.Len()) > bound {
			t.Fatalf("empty stream is %d bytes, bound is %d", buf.Len(), bound)
		}
		decoded, err := io.ReadAll(NewReader(bytes.NewReader(buf.Bytes())))
		if err != nil || len(decoded) != 0 {
			t.Fatalf("empty stream round trip: %v, %d bytes", err, len(decoded))
		}
	}
}

// TestMaxEncodedSizeEmptyFlushes exercises Flush calls that emit no
// meta-block, only the byte-alignment padding block.
func TestMaxEncodedSizeEmptyFlushes(t *testing.T) {
	for _, quality := range []int{0, 1, 2, 6, 11} {
		options := WriterOptions{Quality: quality}
		var buf bytes.Buffer
		w := NewWriterOptions(&buf, WriterOptions{Quality: quality, Bounded: true})
		flushes := 0
		for i := 0; i < 5; i++ {
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			flushes++
		}
		if _, err := w.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		for i := 0; i < 3; i++ {
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			flushes++
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		checkBound(t, []byte("x"), buf.Bytes(), options, flushes)
	}
}

func TestMaxEncodedSizeErrors(t *testing.T) {
	cases := []struct {
		name     string
		size     uint64
		options  WriterOptions
		maxFlush int
	}{
		{"unknown flushes", 100, WriterOptions{Quality: 5}, -1},
		{"quality too low", 100, WriterOptions{Quality: -1}, 0},
		{"quality too high", 100, WriterOptions{Quality: 12}, 0},
		{"lgwin too small", 100, WriterOptions{Quality: 5, LGWin: 9}, 0},
		{"lgwin too large", 100, WriterOptions{Quality: 5, LGWin: 25}, 0},
		{"lgwin negative", 100, WriterOptions{Quality: 5, LGWin: -3}, 0},
		{"size overflow", math.MaxUint64, WriterOptions{Quality: 5}, 0},
		{"size overflow q0", math.MaxUint64, WriterOptions{Quality: 0, LGWin: 10}, 0},
		{"flush overflow", 100, WriterOptions{Quality: 5}, math.MaxInt},
	}
	for _, c := range cases {
		if _, err := MaxEncodedSize(c.size, c.options, c.maxFlush); err == nil {
			t.Errorf("%s: MaxEncodedSize(%d, %+v, %d) succeeded, want error",
				c.name, c.size, c.options, c.maxFlush)
		}
	}

	// Boundary values that must still succeed.
	for _, size := range []uint64{0, 1, 1 << 20, 1 << 40} {
		if _, err := MaxEncodedSize(size, WriterOptions{Quality: 11, LGWin: 24}, 1000); err != nil {
			t.Errorf("MaxEncodedSize(%d, q11/lgwin24, 1000): %v", size, err)
		}
	}
}

// TestMaxEncodedSizeUnboundedQuality2Plus documents that for quality >= 2
// the bound also holds without the Bounded flag.
func TestMaxEncodedSizeUnboundedQuality2Plus(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	data := make([]byte, 1<<17)
	rng.Read(data)
	for _, quality := range []int{2, 6, 11} {
		options := WriterOptions{Quality: quality}
		var buf bytes.Buffer
		w := NewWriterOptions(&buf, options)
		// Odd write pattern with interleaved flushes.
		flushes := 0
		for i := 0; i < len(data); {
			step := 1 + rng.Intn(5000)
			if i+step > len(data) {
				step = len(data) - i
			}
			if _, err := w.Write(data[i : i+step]); err != nil {
				t.Fatalf("Write: %v", err)
			}
			i += step
			if rng.Intn(4) == 0 && i < len(data) {
				if err := w.Flush(); err != nil {
					t.Fatalf("Flush: %v", err)
				}
				flushes++
			}
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		checkBound(t, data, buf.Bytes(), options, flushes)
	}
}
