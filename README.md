This package is a brotli compressor and decompressor implemented in Go.
It was translated from the reference implementation (https://github.com/google/brotli)
with the `c2go` tool at https://github.com/andybalholm/c2go.

I am using it in production with https://github.com/andybalholm/redwood.

API documentation is found at https://pkg.go.dev/github.com/andybalholm/brotli?tab=doc.

## Bounded output size

`MaxEncodedSize(size, options, maxFlushes)` returns a guaranteed upper bound
on the compressed output size for `size` payload bytes, so an output buffer
can be allocated up front instead of estimated from samples. The bound
applies to a `Writer` created with the same options plus `Bounded: true`
(for quality 2 and above it also holds without `Bounded`), with at most
`maxFlushes` calls to `Flush` before `Close`. It is computed as
`size + 6*numBlocks + 2*maxFlushes + 1` bytes, where
`numBlocks = size/minBlockPayload + maxFlushes + 1`; each term maps to a
piece of the Brotli format (stream window-bits header, per-meta-block
ISLAST/MNIBBLES/MLEN/ISUNCOMPRESSED fields plus alignment, the
uncompressed-block fallback, flush padding, and the termination marker) as
documented on `MaxEncodedSize`. Unsupported parameters, an unknown flush
count, or arithmetic overflow are reported as errors rather than a
precise-looking but invalid number.

## Roadmap

I have been working on new compression algorithms (not translated from C)
in the matchfinder package.
You can use them with the NewWriterV2 function.
Currently they give better results than the old implementation
(at least for compressing my test file, Newton’s *Opticks*) 
on levels 0 to 9.

The new APIs are currently considered experimental,
and are not covered by any SemVer compatibility guarantees.
