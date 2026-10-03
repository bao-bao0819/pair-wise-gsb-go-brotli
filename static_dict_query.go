package brotli

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

/* Explainable static dictionary lookup.

   Unlike findAllStaticDictionaryMatches, which is driven by the encoder's
   fast hash table (kStaticDictionaryWords) and only reports final match
   lengths, this index covers every dictionary word combined with every
   RFC 7932 transform and reports exactly which word and which transform
   produced each match, so the matched bytes can be reconstructed. */

/* Maximum length of a transformed dictionary word: the longest word (24)
   plus the longest prefix (" of the ", 8) and the longest suffix (8). */
const maxTransformedDictWordLen = maxDictionaryWordLength + 16

/* Number of bytes of transformed output used as bucket key. */
const staticDictQueryKeyLen = 4

/* StaticDictionaryMatch describes one way the static dictionary can produce
   a byte string: the word WordID of length WordLength, rewritten by
   transform TransformID, yields MatchLength bytes. */
type StaticDictionaryMatch struct {
	WordID      int /* index of the word within its length class */
	WordLength  int /* original dictionary word length in bytes (4..24) */
	TransformID int /* RFC 7932 transform index (0..120) */
	MatchLength int /* length of the transformed word in bytes */
}

/* StaticDictionaryIndex is a read-only index over all (word, transform)
   pairs of the built-in static dictionary. It is built once by
   NewStaticDictionaryIndex and is safe for concurrent lookups.

   Memory: roughly 1.6M entries packed into uint64s (~13 MB) plus hash
   bucket overhead; see Stats for exact figures. Construction evaluates
   13504 words x 121 transforms once, in a single pass. */
type StaticDictionaryIndex struct {
	/* Transformed-output prefix (4 bytes, little endian) -> packed entries. */
	buckets map[uint32][]uint64
	/* Entries whose transformed output is shorter than 4 bytes; scanned
	   linearly because a zero-padded key could not distinguish them from
	   real trailing zero bytes in the input. */
	short      []uint64
	numEntries int
}

func packStaticDictEntry(wordID int, wordLen int, transformID int, matchLen int) uint64 {
	return uint64(wordID) | uint64(wordLen)<<16 | uint64(transformID)<<24 | uint64(matchLen)<<32
}

func unpackStaticDictEntry(e uint64) (wordID int, wordLen int, transformID int, matchLen int) {
	return int(e & 0xFFFF), int((e >> 16) & 0xFF), int((e >> 24) & 0xFF), int((e >> 32) & 0xFF)
}

/* validateStaticDictionaryMatch reports whether the (word, transform)
   combination is well-defined for the built-in dictionary. */
func validateStaticDictionaryMatch(wordLen int, wordID int, transformID int) error {
	dict := getDictionary()
	if wordLen < minDictionaryWordLength || wordLen > maxDictionaryWordLength {
		return fmt.Errorf("brotli: invalid dictionary word length %d (must be %d..%d)",
			wordLen, minDictionaryWordLength, maxDictionaryWordLength)
	}
	if numWords := 1 << uint(dict.size_bits_by_length[wordLen]); wordID < 0 || wordID >= numWords {
		return fmt.Errorf("brotli: invalid dictionary word id %d for length %d (must be 0..%d)",
			wordID, wordLen, numWords-1)
	}
	trans := getTransforms()
	if transformID < 0 || transformID >= int(trans.num_transforms) {
		return fmt.Errorf("brotli: invalid transform id %d (must be 0..%d)",
			transformID, trans.num_transforms-1)
	}
	t := int(transformType(trans, transformID))
	switch {
	case t <= transformOmitLast9:
		if t >= wordLen {
			return fmt.Errorf("brotli: transform %d omits last %d bytes but word length is %d",
				transformID, t, wordLen)
		}
	case t >= transformOmitFirst1 && t <= transformOmitFirst9:
		if skip := t - (transformOmitFirst1 - 1); skip >= wordLen {
			return fmt.Errorf("brotli: transform %d omits first %d bytes but word length is %d",
				transformID, skip, wordLen)
		}
	case t == transformShiftFirst || t == transformShiftAll:
		return fmt.Errorf("brotli: transform %d uses shift parameters, which the RFC 7932 transform set does not define",
			transformID)
	}
	return nil
}

/* transformStaticDictionaryWord writes the transformed word into dst
   (which must hold at least maxTransformedDictWordLen bytes) and returns
   its length. Callers must have validated the combination. */
func transformStaticDictionaryWord(dst []byte, wordLen int, wordID int, transformID int) int {
	dict := getDictionary()
	offset := int(dict.offsets_by_length[wordLen]) + wordID*wordLen
	return transformDictionaryWord(dst, dict.data[offset:offset+wordLen], wordLen, getTransforms(), transformID)
}

/* NewStaticDictionaryIndex builds the explainable lookup index over the
   built-in static dictionary. The result is immutable and safe for
   concurrent use. */
func NewStaticDictionaryIndex() *StaticDictionaryIndex {
	dict := getDictionary()
	idx := &StaticDictionaryIndex{
		buckets: make(map[uint32][]uint64, 1<<16),
	}
	var buf [maxTransformedDictWordLen]byte
	numTransforms := int(getTransforms().num_transforms)
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength; wordLen++ {
		numWords := 1 << uint(dict.size_bits_by_length[wordLen])
		for wordID := 0; wordID < numWords; wordID++ {
			for transformID := 0; transformID < numTransforms; transformID++ {
				if validateStaticDictionaryMatch(wordLen, wordID, transformID) != nil {
					continue
				}
				matchLen := transformStaticDictionaryWord(buf[:], wordLen, wordID, transformID)
				entry := packStaticDictEntry(wordID, wordLen, transformID, matchLen)
				idx.numEntries++
				if matchLen < staticDictQueryKeyLen {
					idx.short = append(idx.short, entry)
					continue
				}
				key := binary.LittleEndian.Uint32(buf[:])
				idx.buckets[key] = append(idx.buckets[key], entry)
			}
		}
	}
	return idx
}

/* sortStaticDictionaryMatches orders matches deterministically so that
   duplicate byte strings from different (word, transform) sources are all
   kept in a stable order. */
func sortStaticDictionaryMatches(matches []StaticDictionaryMatch) {
	sort.Slice(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.WordLength != b.WordLength {
			return a.WordLength < b.WordLength
		}
		if a.WordID != b.WordID {
			return a.WordID < b.WordID
		}
		return a.TransformID < b.TransformID
	})
}

/* Lookup returns every (word, transform) combination whose transformed
   output is a prefix of input, sorted by (WordLength, WordID, TransformID).
   Several matches may describe the same byte string when it has multiple
   dictionary sources. */
func (idx *StaticDictionaryIndex) Lookup(input []byte) []StaticDictionaryMatch {
	var matches []StaticDictionaryMatch
	var buf [maxTransformedDictWordLen]byte
	check := func(entry uint64) {
		wordID, wordLen, transformID, matchLen := unpackStaticDictEntry(entry)
		if matchLen > len(input) {
			return
		}
		n := transformStaticDictionaryWord(buf[:], wordLen, wordID, transformID)
		if n != matchLen {
			return
		}
		if bytes.Equal(buf[:matchLen], input[:matchLen]) {
			matches = append(matches, StaticDictionaryMatch{
				WordID:      wordID,
				WordLength:  wordLen,
				TransformID: transformID,
				MatchLength: matchLen,
			})
		}
	}
	if len(input) >= staticDictQueryKeyLen {
		for _, entry := range idx.buckets[binary.LittleEndian.Uint32(input)] {
			check(entry)
		}
	}
	for _, entry := range idx.short {
		check(entry)
	}
	sortStaticDictionaryMatches(matches)
	return matches
}

/* StaticDictionaryIndexStats reports the size of a StaticDictionaryIndex. */
type StaticDictionaryIndexStats struct {
	NumEntries      int   /* total (word, transform) pairs indexed */
	NumBuckets      int   /* distinct 4-byte prefix buckets */
	NumShortEntries int   /* entries with transformed output shorter than 4 bytes */
	ApproxBytes     int64 /* rough memory footprint of the index */
}

/* Stats returns size and memory estimates for the index. ApproxBytes counts
   the packed entries, bucket slice headers, and hash map overhead. */
func (idx *StaticDictionaryIndex) Stats() StaticDictionaryIndexStats {
	stats := StaticDictionaryIndexStats{
		NumEntries:      idx.numEntries,
		NumBuckets:      len(idx.buckets),
		NumShortEntries: len(idx.short),
	}
	stats.ApproxBytes = int64(idx.numEntries)*8 + /* packed entries */
		int64(len(idx.buckets))*64 /* slice header + map slot overhead */
	return stats
}

/* ReconstructDictionaryMatch rebuilds the matched bytes described by m.
   It returns an error for unknown word ids, unknown transform ids, invalid
   (word, transform) combinations (e.g. omitting more bytes than the word
   has), or a MatchLength inconsistent with the other fields. */
func ReconstructDictionaryMatch(m StaticDictionaryMatch) ([]byte, error) {
	if err := validateStaticDictionaryMatch(m.WordLength, m.WordID, m.TransformID); err != nil {
		return nil, err
	}
	buf := make([]byte, maxTransformedDictWordLen)
	n := transformStaticDictionaryWord(buf, m.WordLength, m.WordID, m.TransformID)
	if m.MatchLength != n {
		return nil, fmt.Errorf("brotli: match length %d inconsistent with reconstructed length %d",
			m.MatchLength, n)
	}
	return buf[:n], nil
}
