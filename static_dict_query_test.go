package brotli

import (
	"bytes"
	"reflect"
	"sync"
	"testing"
)

/* naiveStaticDictionaryLookup is the reference implementation: it scans
   every dictionary word combined with every transform. */
func naiveStaticDictionaryLookup(input []byte) []StaticDictionaryMatch {
	dict := getDictionary()
	var matches []StaticDictionaryMatch
	var buf [maxTransformedDictWordLen]byte
	numTransforms := int(getTransforms().num_transforms)
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength; wordLen++ {
		numWords := 1 << uint(dict.size_bits_by_length[wordLen])
		for wordID := 0; wordID < numWords; wordID++ {
			for transformID := 0; transformID < numTransforms; transformID++ {
				if validateStaticDictionaryMatch(wordLen, wordID, transformID) != nil {
					continue
				}
				n := transformStaticDictionaryWord(buf[:], wordLen, wordID, transformID)
				if n <= len(input) && bytes.Equal(buf[:n], input[:n]) {
					matches = append(matches, StaticDictionaryMatch{
						WordID:      wordID,
						WordLength:  wordLen,
						TransformID: transformID,
						MatchLength: n,
					})
				}
			}
		}
	}
	sortStaticDictionaryMatches(matches)
	return matches
}

func dictionaryWord(wordLen int, wordID int) []byte {
	dict := getDictionary()
	offset := int(dict.offsets_by_length[wordLen]) + wordID*wordLen
	return dict.data[offset : offset+wordLen]
}

/* findTransformID returns the first transform with the given prefix, type
   and suffix, or -1. */
func findTransformID(prefix string, typ byte, suffix string) int {
	trans := getTransforms()
	for i := 0; i < int(trans.num_transforms); i++ {
		if transformType(trans, i) != typ {
			continue
		}
		p := transformPrefix(trans, i)
		if int(p[0]) != len(prefix) || !bytes.Equal(p[1:1+len(prefix)], []byte(prefix)) {
			continue
		}
		s := transformSuffix(trans, i)
		if int(s[0]) != len(suffix) || !bytes.Equal(s[1:1+len(suffix)], []byte(suffix)) {
			continue
		}
		return i
	}
	return -1
}

/* sampleDictionaryInputs builds probe inputs from real dictionary words,
   covering identity, truncation, prefix/suffix and multi-byte uppercase
   transforms. */
func sampleDictionaryInputs(t *testing.T) [][]byte {
	t.Helper()
	dict := getDictionary()

	/* Pick words: first of each length, plus words containing multi-byte
	   UTF-8 sequences to exercise the format-defined uppercasing. */
	var words [][]byte
	multiByte := 0
	for wordLen := minDictionaryWordLength; wordLen <= maxDictionaryWordLength; wordLen++ {
		words = append(words, dictionaryWord(wordLen, 0))
		numWords := 1 << uint(dict.size_bits_by_length[wordLen])
		for wordID := 0; wordID < numWords && multiByte < 2; wordID++ {
			w := dictionaryWord(wordLen, wordID)
			hasMultiByte := false
			for _, b := range w {
				if b >= 0xC0 {
					hasMultiByte = true
					break
				}
			}
			if hasMultiByte {
				words = append(words, w)
				multiByte++
			}
		}
	}
	if multiByte == 0 {
		t.Fatal("no multi-byte UTF-8 words found in dictionary")
	}

	transformIDs := []int{
		findTransformID("", transformIdentity, ""),
		findTransformID("", transformOmitLast1, ""),
		findTransformID("", transformOmitLast3, ""),
		findTransformID("", transformOmitLast9, ""),
		findTransformID("", transformUppercaseFirst, ""),
		findTransformID("", transformUppercaseAll, ""),
		findTransformID(" ", transformUppercaseFirst, ""),
		findTransformID(" ", transformUppercaseAll, ""),
		findTransformID(" the ", transformIdentity, ""),
		findTransformID("", transformOmitLast1, "ing "),
		findTransformID(".com/", transformIdentity, ""),
	}
	for _, id := range transformIDs {
		if id < 0 {
			t.Fatal("expected transform not found in kTransformsData")
		}
	}

	var inputs [][]byte
	var buf [maxTransformedDictWordLen]byte
	for _, w := range words {
		wordLen := len(w)
		for _, transformID := range transformIDs {
			wordID := -1
			numWords := 1 << uint(dict.size_bits_by_length[wordLen])
			offset := int(dict.offsets_by_length[wordLen])
			for id := 0; id < numWords; id++ {
				if bytes.Equal(dict.data[offset+id*wordLen:offset+(id+1)*wordLen], w) {
					wordID = id
					break
				}
			}
			if wordID < 0 || validateStaticDictionaryMatch(wordLen, wordID, transformID) != nil {
				continue
			}
			n := transformStaticDictionaryWord(buf[:], wordLen, wordID, transformID)
			out := make([]byte, n)
			copy(out, buf[:n])
			inputs = append(inputs, out)
			/* Also probe with trailing bytes to confirm prefix matching. */
			inputs = append(inputs, append(append([]byte{}, out...), 'x', 'y', 'z'))
		}
	}
	inputs = append(inputs,
		[]byte(""),
		[]byte("a"),
		[]byte("time"),
		[]byte("TIME"),
		[]byte(" the download"),
		[]byte("\xc2\xa0"),
	)
	return inputs
}

func TestStaticDictionaryIndexMatchesNaive(t *testing.T) {
	idx := NewStaticDictionaryIndex()
	stats := idx.Stats()
	t.Logf("index stats: %+v", stats)
	if stats.NumEntries == 0 || stats.NumBuckets == 0 {
		t.Fatal("index is empty")
	}
	inputs := sampleDictionaryInputs(t)
	checked := 0
	for i, input := range inputs {
		/* The naive reference scans the whole dictionary per input, so
		   only compare against a representative subset: all multi-byte
		   (UTF-8 transform) inputs, a strided sample, and the fixed edge
		   cases at the end. */
		multiByte := false
		for _, b := range input {
			if b >= 0x80 {
				multiByte = true
				break
			}
		}
		if !multiByte && i%53 != 0 && i < len(inputs)-6 {
			continue
		}
		checked++
		got := idx.Lookup(input)
		want := naiveStaticDictionaryLookup(input)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Lookup(%q) mismatch:\n got %v\nwant %v", input, got, want)
		}
		for _, m := range got {
			if m.MatchLength > len(input) {
				t.Fatalf("Lookup(%q): match %+v longer than input", input, m)
			}
		}
	}
	t.Logf("naive-compared %d of %d inputs", checked, len(inputs))
}

func TestStaticDictionaryLookupDuplicateSources(t *testing.T) {
	idx := NewStaticDictionaryIndex()
	dict := getDictionary()

	/* Find two words w (length l) and w' (length l+1) with w'[:l] == w, so
	   that the identity transform of w and the omit-last-1 transform of w'
	   produce the same byte string. */
	omitLast1 := findTransformID("", transformOmitLast1, "")
	identity := findTransformID("", transformIdentity, "")
	found := false
	for wordLen := minDictionaryWordLength; wordLen < maxDictionaryWordLength && !found; wordLen++ {
		numWords := 1 << uint(dict.size_bits_by_length[wordLen])
		numLonger := 1 << uint(dict.size_bits_by_length[wordLen+1])
		for id := 0; id < numWords && !found; id++ {
			w := dictionaryWord(wordLen, id)
			for id2 := 0; id2 < numLonger; id2++ {
				if bytes.Equal(dictionaryWord(wordLen+1, id2)[:wordLen], w) {
					matches := idx.Lookup(w)
					var sawIdentity, sawOmit bool
					for _, m := range matches {
						if m.WordLength == wordLen && m.WordID == id && m.TransformID == identity {
							sawIdentity = true
						}
						if m.WordLength == wordLen+1 && m.WordID == id2 && m.TransformID == omitLast1 {
							sawOmit = true
						}
					}
					if !sawIdentity || !sawOmit {
						t.Fatalf("Lookup(%q): identity=%v omit=%v, want both sources kept", w, sawIdentity, sawOmit)
					}
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Fatal("no overlapping word pair found in dictionary")
	}
}

func TestReconstructDictionaryMatchRoundTrip(t *testing.T) {
	idx := NewStaticDictionaryIndex()
	for _, input := range sampleDictionaryInputs(t) {
		for _, m := range idx.Lookup(input) {
			got, err := ReconstructDictionaryMatch(m)
			if err != nil {
				t.Fatalf("ReconstructDictionaryMatch(%+v): %v", m, err)
			}
			if !bytes.Equal(got, input[:m.MatchLength]) {
				t.Fatalf("ReconstructDictionaryMatch(%+v) = %q, want %q", m, got, input[:m.MatchLength])
			}
		}
	}
}

func TestReconstructDictionaryMatchErrors(t *testing.T) {
	valid := StaticDictionaryMatch{WordID: 0, WordLength: 4, TransformID: findTransformID("", transformIdentity, ""), MatchLength: 4}
	if _, err := ReconstructDictionaryMatch(valid); err != nil {
		t.Fatalf("valid match rejected: %v", err)
	}

	cases := []StaticDictionaryMatch{
		{WordID: 0, WordLength: 3, TransformID: 0, MatchLength: 3},   /* length too small */
		{WordID: 0, WordLength: 25, TransformID: 0, MatchLength: 25}, /* length too large */
		{WordID: 1 << 10, WordLength: 4, TransformID: 0, MatchLength: 4},
		{WordID: -1, WordLength: 4, TransformID: 0, MatchLength: 4},
		{WordID: 0, WordLength: 4, TransformID: 121, MatchLength: 4},
		{WordID: 0, WordLength: 4, TransformID: -1, MatchLength: 4},
		/* Omit more than the word has. */
		{WordID: 0, WordLength: 4, TransformID: findTransformID("", transformOmitLast9, ""), MatchLength: 0},
		{WordID: 0, WordLength: 4, TransformID: findTransformID("", transformOmitLast4, ""), MatchLength: 0},
		/* Inconsistent match length. */
		{WordID: 0, WordLength: 4, TransformID: valid.TransformID, MatchLength: 5},
	}
	for i, m := range cases {
		if _, err := ReconstructDictionaryMatch(m); err == nil {
			t.Fatalf("case %d: %+v: expected error, got nil", i, m)
		}
	}
}

func TestStaticDictionaryIndexConcurrent(t *testing.T) {
	idx := NewStaticDictionaryIndex()
	inputs := sampleDictionaryInputs(t)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2; i++ {
				input := inputs[(g*2+i)%len(inputs)]
				got := idx.Lookup(input)
				want := naiveStaticDictionaryLookup(input)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("Lookup(%q) mismatch", input)
				}
			}
		}(g)
	}
	wg.Wait()
}
