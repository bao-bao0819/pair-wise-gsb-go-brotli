package brotli

import (
	"errors"
	"fmt"
	"math"
)

// minNonForcedBlockPayload is a lower bound on the number of payload bytes
// in any meta-block that the quality >= 2 encoder emits without being
// forced by Flush or Close:
//
//   - Quality 2 and 3 flush a pending meta-block only when
//     num_literals+num_commands reaches maxNumDelayedSymbols (0x2FFF).
//     Every command covers at least one input byte (insert-only commands
//     insert >= 1 literal; copy lengths are >= 2), so the block holds at
//     least maxNumDelayedSymbols/2 bytes.
//   - Quality >= 4 flushes only when the pending meta-block exceeds
//     maxMetablockSize-inputBlockSize bytes, or accumulates
//     maxMetablockSize/8 literals or commands; with the supported
//     parameter range (LGWin 10..24, LgBlock 16..18) all of these
//     thresholds are >= 1<<14.
const minNonForcedBlockPayload = maxNumDelayedSymbols/2 + 1

// maxMetaBlockOverheadBits is an upper bound on the number of bits a single
// meta-block costs on top of 8 bits per payload byte:
//
//	ISLAST (1) + ISEMPTY (1) + MNIBBLES (2) + MLEN (up to 24) +
//	ISUNCOMPRESSED (1) + byte-alignment padding (up to 7) +
//	empty final meta-block marker appended after an uncompressed last
//	block (2 + up to 7 padding) = 44 bits.
//
// The encoder guarantees this bound for every meta-block by falling back to
// a legal uncompressed meta-block whenever the compressed representation
// would be larger: compressFragmentFast and compressFragmentTwoPass rewrite
// any fragment that exceeds 31+8*n bits, and writeMetaBlockInternal
// re-emits any meta-block whose compressed form exceeds payload+4 bytes as
// an uncompressed meta-block. Output is never truncated.
const maxMetaBlockOverheadBits = 44

// maxFlushPaddingBits is an upper bound on the bits a forced Flush adds on
// top of the meta-blocks it terminates: flushing a partial byte emits an
// empty metadata block of 6 bits (ISLAST=0, MNIBBLES=11, reserved=0,
// MSKIPBYTES=0) starting at a bit offset of at most 7, i.e. at most 13
// bits.
const maxFlushPaddingBits = 13

// maxStreamHeaderBits is an upper bound on the window-bits stream header
// for LGWin 10..24 (large-window mode is not supported by Writer).
const maxStreamHeaderBits = 7

var (
	errBoundOverflow = errors.New("brotli: MaxEncodedSize: bound overflows int and cannot be used to size a buffer")
	errUnknownFlush  = errors.New("brotli: MaxEncodedSize: maxFlushes is negative; the number of flushes must be known to compute a guaranteed bound")
)

// MaxEncodedSize returns a conservative upper bound, in bytes, on the size
// of the compressed output produced for size bytes of payload written to a
// Writer created with NewWriterOptions using the given options with Bounded
// set to true, followed by at most maxFlushes calls to Flush and one call
// to Close. The bound holds for any Write call pattern (any number and
// sizes of Write calls); it does not apply to writers created by
// NewWriterV2. For quality levels 2 and above the bound also holds for
// writers without Bounded, since bounded mode only changes the behavior of
// the quality 0 and 1 fast paths.
//
// The bound is computed as:
//
//	numBlocks = size/minBlockPayload + maxFlushes + 1
//	bound     = size + 6*numBlocks + 2*maxFlushes + 1
//
// where minBlockPayload is 1<<LGWin for quality 0 and 1 (the segment size
// used by bounded mode, LGWin defaults to 22) and minNonForcedBlockPayload
// (6144) for quality >= 2. Each term corresponds to a piece of the Brotli
// format (RFC 7932):
//
//   - size: every meta-block costs at most its payload plus header,
//     because the encoder falls back to a legal uncompressed meta-block
//     whenever the compressed representation would be larger (see
//     maxMetaBlockOverheadBits).
//   - 6 bytes per meta-block: ISLAST + ISEMPTY + MNIBBLES + MLEN (up to 24
//     bits) + ISUNCOMPRESSED + alignment padding + the empty final marker
//     after an uncompressed last block, at most 44 bits < 6 bytes.
//   - 2 bytes per Flush: the empty metadata padding block emitted to align
//     the stream to a byte boundary, at most 13 bits < 2 bytes.
//   - 1 byte: the window-bits stream header (at most 7 bits) plus the final
//     round-up to a whole byte.
//
// The number of meta-blocks is bounded because every meta-block emitted
// without a forced Flush or Close carries at least minBlockPayload bytes,
// each Flush terminates at most one extra meta-block, and Close terminates
// exactly one final meta-block (possibly empty, which is why a zero size
// still yields a non-zero bound).
//
// An error is returned instead of a misleadingly precise-looking number
// when the parameters are unsupported (Quality outside 0..11, LGWin neither
// 0 nor 10..24), when the flush count is unknown (maxFlushes < 0), or when
// the bound overflows the uint64/int arithmetic needed to size a buffer.
func MaxEncodedSize(size uint64, options WriterOptions, maxFlushes int) (uint64, error) {
	if maxFlushes < 0 {
		return 0, errUnknownFlush
	}
	if options.Quality < minQuality || options.Quality > maxQuality {
		return 0, fmt.Errorf("brotli: MaxEncodedSize: unsupported quality %d (must be %d..%d)", options.Quality, minQuality, maxQuality)
	}
	if options.LGWin != 0 && (options.LGWin < minWindowBits || options.LGWin > maxWindowBits) {
		return 0, fmt.Errorf("brotli: MaxEncodedSize: unsupported LGWin %d (must be 0 or %d..%d)", options.LGWin, minWindowBits, maxWindowBits)
	}

	minBlockPayload := uint64(minNonForcedBlockPayload)
	if options.Quality <= fastTwoPassCompressionQuality {
		minBlockPayload = 1 << uint(sanitizedLGWin(options.LGWin))
	}

	flushes := uint64(maxFlushes)
	const maxUint64 = ^uint64(0)

	// numBlocks = size/minBlockPayload + flushes + 1. All additions and
	// multiplications below are checked for uint64 overflow.
	numBlocks := size / minBlockPayload
	if numBlocks > maxUint64-flushes-1 {
		return 0, errBoundOverflow
	}
	numBlocks += flushes + 1

	// bound = size + 6*numBlocks + 2*flushes + 1, i.e. the bit-level bound
	//	7 + 8*size + 44*numBlocks + 13*flushes
	// rounded up to whole bytes.
	if numBlocks > (maxUint64-1)/6 {
		return 0, errBoundOverflow
	}
	blockOverhead := 6*numBlocks + 1
	if flushes > (maxUint64-blockOverhead)/2 {
		return 0, errBoundOverflow
	}
	overhead := blockOverhead + 2*flushes
	if size > maxUint64-overhead {
		return 0, errBoundOverflow
	}
	bound := size + overhead
	if bound > uint64(math.MaxInt) {
		return 0, errBoundOverflow
	}
	return bound, nil
}

// sanitizedQuality mirrors the clamping sanitizeParams applies to Quality.
func sanitizedQuality(quality int) int {
	if quality < minQuality {
		return minQuality
	}
	if quality > maxQuality {
		return maxQuality
	}
	return quality
}

// sanitizedLGWin mirrors the effective LGWin of a Writer: Reset ignores
// non-positive values (leaving the default) and sanitizeParams clamps the
// rest to 10..24.
func sanitizedLGWin(lgwin int) int {
	if lgwin <= 0 {
		return defaultWindow
	}
	if lgwin < minWindowBits {
		return minWindowBits
	}
	if lgwin > maxWindowBits {
		return maxWindowBits
	}
	return lgwin
}
