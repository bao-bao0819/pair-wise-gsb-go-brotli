package brotli

import (
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/andybalholm/brotli/matchfinder"
)

// This file provides a provable upper bound on the size of Brotli-encoded
// output (MaxEncodedSize) and an opt-in encoding mode (BoundedWriter) that
// the bound applies to.
//
// # Why a separate encoding mode
//
// The classic Writer splits the input into meta-blocks using internal
// heuristics, and its quality 0/1 fast paths have no uncompressed fallback,
// so no tight output-size bound has been proven for it. BoundedWriter
// instead cuts the stream into meta-blocks of at most boundedChunkSize
// bytes and, for every meta-block, emits the compressed representation only
// when it is strictly smaller than the legal uncompressed representation of
// the same bytes. That per-block fallback is what makes the bound a
// guarantee rather than an estimate. Output is never truncated: when a
// locally compressed block would exceed the bound, a legal uncompressed
// meta-block is emitted instead.
//
// # The bound, term by term
//
// For a payload of n bytes and at most f forced Flush calls, the encoded
// stream produced by BoundedWriter is at most
//
//	ceil((7 + 35*B + 8*n + 2) / 8)  bytes,  where B = ceil(n / 2^20) + f
//
// Each term corresponds to a piece of the RFC 7932 container format:
//
//   - 7 bits: the stream header (WBITS). For lgwin 10..24 the encoding is
//     1, 4 or 7 bits; 7 is the worst case.
//   - 28 bits per meta-block: the meta-block header — ISLAST (1 bit),
//     MNIBBLES (2 bits), MLEN-1 (up to 24 bits, i.e. 6 nibbles),
//     ISUNCOMPRESSED (1 bit).
//   - 7 bits per meta-block: zero padding to the next byte boundary, which
//     the format requires between an uncompressed meta-block header and its
//     raw payload. Together with the header this is 35 bits per block.
//   - 8*n bits: the payload itself. A meta-block is emitted compressed only
//     if its compressed size is strictly smaller than the uncompressed
//     representation of the same block, so every block costs at most its
//     raw bytes plus the header above.
//   - 2 bits: the stream termination marker (ISLAST=1, ISLASTEMPTY=1).
//   - The final ceil-to-byte adds at most 7 more bits of padding, which the
//     format allows at the end of the stream.
//
// The block count B is bounded because BoundedWriter starts a new
// meta-block only when boundedChunkSize (2^20) payload bytes have
// accumulated, when Flush is called with pending data, or at Close; flushes
// with no pending data emit nothing.

const (
	// boundedChunkSize is the maximum number of payload bytes covered by a
	// single meta-block produced by BoundedWriter.
	boundedChunkSize = 1 << 20

	// maxStreamHeaderBits bounds the WBITS stream header for lgwin 10..24.
	maxStreamHeaderBits = 7
	// maxMetaBlockOverheadBits bounds a meta-block header (ISLAST, MNIBBLES,
	// MLEN, ISUNCOMPRESSED: 1+2+24+1 = 28 bits) plus the up-to-7-bit byte
	// alignment padding that precedes an uncompressed payload.
	maxMetaBlockOverheadBits = 28 + 7
	// streamTrailerBits is the final ISLAST+ISLASTEMPTY marker.
	streamTrailerBits = 2
)

var (
	errBoundOverflow  = errors.New("brotli: MaxEncodedSize: size bound overflows uint64")
	errUnknownFlushes = errors.New("brotli: MaxEncodedSize: maxFlushes must be a non-negative upper bound on the number of Flush calls")
)

// MaxEncodedSize returns a conservative, guaranteed upper bound on the
// number of bytes produced when encoding srcLen payload bytes with a
// BoundedWriter using the given options, provided Flush is called at most
// maxFlushes times and Close is called exactly once at the end.
//
// The bound applies only to BoundedWriter (NewBoundedWriter) — see the
// documentation in bounded.go for which Writer behaviors are covered and
// for the per-term derivation of the formula. It does not apply to Writer
// or the writers returned by NewWriterV2.
//
// An error is returned instead of a misleading number when the bound cannot
// be guaranteed: unsupported encoding parameters (Quality outside 0..11,
// LGWin outside 0 or 10..24), an unknown number of flushes (maxFlushes < 0),
// or arithmetic overflow while computing the bound. All additions,
// multiplications and the final round-up are checked for uint64 overflow.
// On 32-bit platforms the result may not fit in an int; callers should
// compare it against math.MaxInt before using it to size an allocation.
//
// A zero srcLen is valid: the result covers the stream header and the
// termination marker of an empty stream.
func MaxEncodedSize(srcLen uint64, options WriterOptions, maxFlushes int64) (uint64, error) {
	if options.Quality < 0 || options.Quality > BestCompression {
		return 0, fmt.Errorf("brotli: MaxEncodedSize: unsupported quality %d (must be between %d and %d)", options.Quality, BestSpeed, BestCompression)
	}
	if options.LGWin != 0 && (options.LGWin < 10 || options.LGWin > 24) {
		return 0, fmt.Errorf("brotli: MaxEncodedSize: unsupported lgwin %d (must be 0 or between 10 and 24)", options.LGWin)
	}
	if maxFlushes < 0 {
		return 0, errUnknownFlushes
	}

	// Number of meta-blocks: full chunks plus one per non-empty Flush and
	// one for the final partial chunk emitted by Close.
	blocks := srcLen / boundedChunkSize
	if srcLen%boundedChunkSize != 0 {
		blocks++
	}
	var carry uint64
	if blocks, carry = bits.Add64(blocks, uint64(maxFlushes), 0); carry != 0 {
		return 0, errBoundOverflow
	}

	// Payload bits.
	hi, total := bits.Mul64(srcLen, 8)
	if hi != 0 {
		return 0, errBoundOverflow
	}
	// Per-block header + alignment bits.
	hi, overhead := bits.Mul64(blocks, maxMetaBlockOverheadBits)
	if hi != 0 {
		return 0, errBoundOverflow
	}
	if total, carry = bits.Add64(total, overhead, 0); carry != 0 {
		return 0, errBoundOverflow
	}
	// Stream header, termination marker, and the final round-up to a whole
	// byte (the +7) are all accounted for here.
	if total, carry = bits.Add64(total, maxStreamHeaderBits+streamTrailerBits+7, 0); carry != 0 {
		return 0, errBoundOverflow
	}
	return total / 8, nil
}

// uncompressedBlockBits returns the exact number of bits that emitting n
// payload bytes as an uncompressed meta-block would append to a stream
// whose current bit position modulo 8 is bitPos.
func uncompressedBlockBits(bitPos uint, n int) uint64 {
	// This mirrors the nibble selection of storeMetaBlockHeaderBW.
	mnibbles := uint64(4)
	if n > 1<<16 {
		mnibbles = 5
	}
	if n > 1<<20 {
		mnibbles = 6
	}
	header := 1 + 2 + 4*mnibbles + 1 // ISLAST + MNIBBLES + MLEN + ISUNCOMPRESSED
	pad := (8 - (uint64(bitPos)+header)%8) % 8
	return header + pad + 8*uint64(n)
}

// A BoundedWriter compresses data in Brotli format while guaranteeing the
// output-size bound computed by MaxEncodedSize: whenever a meta-block would
// compress to more bits than its legal uncompressed representation, the
// uncompressed representation is emitted instead. Output is never
// truncated.
//
// The guarantee holds for any sequence of Write calls, at most maxFlushes
// calls to Flush, and a single final Close, with the WriterOptions given to
// NewBoundedWriter. Options outside the supported ranges are normalized
// (Quality is clamped to 0..11, LGWin to 10..24 with 0 meaning the default
// window); MaxEncodedSize rejects such options instead, so pass validated
// options to both.
type BoundedWriter struct {
	dst         io.Writer
	mf          matchfinder.MatchFinder
	enc         Encoder
	lgwin       int
	bw          bitWriter // stream bits not yet drained to dst
	scratch     bitWriter // scratch space for the compressed-block attempt
	pending     []byte    // payload bytes not yet covered by a meta-block
	matches     []matchfinder.Match
	err         error
	closed      bool
	wroteHeader bool
}

// NewBoundedWriter returns a BoundedWriter writing Brotli-compressed data
// to dst, configured by options (see WriterOptions). Qualities 10 and 11
// use the same match finder as quality 9, mirroring NewWriterV2.
func NewBoundedWriter(dst io.Writer, options WriterOptions) *BoundedWriter {
	quality := options.Quality
	if quality < 0 {
		quality = 0
	}
	if quality > 9 {
		quality = 9
	}
	lgwin := options.LGWin
	if lgwin == 0 {
		lgwin = defaultWindow
	}
	if lgwin < 10 {
		lgwin = 10
	}
	if lgwin > 24 {
		lgwin = 24
	}
	// The maximum legal backward distance for a stream is (1<<lgwin) - 16.
	maxDistance := (1 << uint(lgwin)) - 16
	var mf matchfinder.MatchFinder
	switch quality {
	case 0, 1:
		mf = &matchfinder.ZFast{MaxDistance: maxDistance}
	case 2:
		mf = &matchfinder.ZDFast{MaxDistance: maxDistance}
	case 3:
		mf = &matchfinder.ZM{MaxDistance: maxDistance}
	case 4:
		mf = &matchfinder.Trio{MaxDistance: maxDistance}
	case 5, 6:
		mf = &matchfinder.Bargain1{MaxDistance: maxDistance}
	case 7:
		mf = &matchfinder.Bargain2{MaxDistance: maxDistance, Skip: true}
	case 8:
		mf = &matchfinder.Bargain2{MaxDistance: maxDistance}
	default:
		mf = &matchfinder.Bargain3{MaxDistance: maxDistance}
	}
	return &BoundedWriter{dst: dst, mf: mf, lgwin: lgwin}
}

// writeHeader emits the WBITS stream header.
func (w *BoundedWriter) writeHeader() {
	var hdr uint16
	var hdrBits byte
	encodeWindowBits(w.lgwin, false, &hdr, &hdrBits)
	w.bw.writeBits(uint(hdrBits), uint64(hdr))
	w.wroteHeader = true
}

// emitBlock emits src (1..boundedChunkSize bytes) as a single meta-block,
// choosing the smaller of the compressed and uncompressed representations.
func (w *BoundedWriter) emitBlock(src []byte) {
	if !w.wroteHeader {
		w.writeHeader()
	}

	w.matches = w.mf.FindMatches(w.matches[:0], src)
	w.scratch = bitWriter{dst: w.scratch.dst[:0]}
	w.enc.encodeMetaBlock(&w.scratch, src, w.matches)

	if w.scratch.totalBits() < uncompressedBlockBits(w.bw.nbits%8, len(src)) {
		// The compressed block is smaller: splice its exact bit sequence
		// into the stream.
		for _, b := range w.scratch.dst {
			w.bw.writeBits(8, uint64(b))
		}
		if w.scratch.nbits > 0 {
			w.bw.writeBits(w.scratch.nbits, w.scratch.bits)
		}
	} else {
		// Fall back to a legal uncompressed meta-block so the local
		// encoding never exceeds the proven bound.
		storeMetaBlockHeaderBW(uint(len(src)), true, &w.bw)
		w.bw.jumpToByteBoundary()
		w.bw.dst = append(w.bw.dst, src...)
	}

	// Push all complete bytes to the underlying writer so that Flush
	// semantics hold; at most 7 bits may remain buffered.
	w.bw.flushWholeBytes()
	w.writeOut()
}

func (w *BoundedWriter) writeOut() {
	if w.err != nil || len(w.bw.dst) == 0 {
		return
	}
	_, w.err = w.dst.Write(w.bw.dst)
	w.bw.dst = w.bw.dst[:0]
}

// Write implements io.Writer. Flush or Close must be called to ensure that
// the encoded bytes are actually flushed to the underlying writer.
func (w *BoundedWriter) Write(p []byte) (n int, err error) {
	if w.closed {
		return 0, errWriterClosed
	}
	if w.err != nil {
		return 0, w.err
	}
	w.pending = append(w.pending, p...)
	for len(w.pending) >= boundedChunkSize {
		w.emitBlock(w.pending[:boundedChunkSize])
		copy(w.pending, w.pending[boundedChunkSize:])
		w.pending = w.pending[:len(w.pending)-boundedChunkSize]
		if w.err != nil {
			return 0, w.err
		}
	}
	return len(p), nil
}

// Flush outputs encoded data for all input provided to Write. The resulting
// output can be decoded to match all input before Flush, but the stream is
// not yet complete until after Close. A Flush with no pending input emits
// nothing. Flush has a negative impact on compression.
func (w *BoundedWriter) Flush() error {
	if w.closed {
		return errWriterClosed
	}
	if w.err != nil {
		return w.err
	}
	if len(w.pending) > 0 {
		w.emitBlock(w.pending)
		w.pending = w.pending[:0]
	}
	return w.err
}

// Close flushes remaining data and the stream termination marker to the
// decorated writer. It must be called exactly once at the end of the
// stream for the MaxEncodedSize bound to apply.
func (w *BoundedWriter) Close() error {
	if w.closed {
		return nil
	}
	if w.err == nil {
		if len(w.pending) > 0 {
			w.emitBlock(w.pending)
			w.pending = w.pending[:0]
		}
		if w.err == nil {
			if !w.wroteHeader {
				w.writeHeader()
			}
			w.bw.writeBits(2, 3) // ISLAST + ISLASTEMPTY
			w.bw.jumpToByteBoundary()
			w.writeOut()
		}
	}
	w.closed = true
	return w.err
}
