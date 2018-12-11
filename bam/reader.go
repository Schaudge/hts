// Copyright ©2012 The bíogo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package bam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"unsafe"

	"github.com/grailbio/hts/bgzf"
	"github.com/grailbio/hts/sam"
)

// Reader implements BAM data reading.
type Reader struct {
	r *bgzf.Reader
	h *sam.Header
	c *bgzf.Chunk

	// references is cached header
	// reference count.
	references int32

	// omit specifies how much of the
	// record should be omitted during
	// a read of the BAM input.
	omit int

	lastChunk bgzf.Chunk

	// sizeBuf and sizeStorage are used to read the block size of each record
	// without having to allocate new storage and a slice everytime.
	sizeStorage [4]byte
	sizeBuf     []byte
}

const maxBAMRecordSize = 0xffffff

// NewReader returns a new Reader using the given io.Reader
// and setting the read concurrency to rd. If rd is zero
// concurrency is set to GOMAXPROCS. The returned Reader
// should be closed after use to avoid leaking resources.
func NewReader(r io.Reader, rd int) (*Reader, error) {
	bg, err := bgzf.NewReader(r, rd)
	if err != nil {
		return nil, err
	}
	h, _ := sam.NewHeader(nil, nil)
	br := &Reader{
		r: bg,
		h: h,

		references: int32(len(h.Refs())),
	}
	err = br.h.DecodeBinary(br.r)
	if err != nil {
		return nil, err
	}
	br.lastChunk.End = br.r.LastChunk().End
	br.sizeBuf = br.sizeStorage[:]
	return br, nil
}

// Header returns the SAM Header held by the Reader.
func (br *Reader) Header() *sam.Header {
	return br.h
}

// BAM record layout.
type bamRecordFixed struct {
	blockSize int32
	refID     int32
	pos       int32
	nLen      uint8
	mapQ      uint8
	bin       uint16
	nCigar    uint16
	flags     sam.Flags
	lSeq      int32
	nextRefID int32
	nextPos   int32
	tLen      int32
}

var (
	lenFieldSize      = binary.Size(bamRecordFixed{}.blockSize)
	bamFixedRemainder = binary.Size(bamRecordFixed{}) - lenFieldSize
)

func vOffset(o bgzf.Offset) int64 {
	return o.File<<16 | int64(o.Block)
}

// Omit specifies what portions of the Record to omit reading.
// When o is None, a full sam.Record is returned by Read, when o
// is AuxTags the auxiliary tag data is omitted and when o is
// AllVariableLengthData, sequence, quality and auxiliary data
// is omitted.
func (br *Reader) Omit(o int) {
	br.omit = o
}

// None, AuxTags and AllVariableLengthData are values taken
// by the Reader Omit method.
const (
	None                  = iota // Omit no field data from the record.
	AuxTags                      // Omit auxiliary tag data.
	AllVariableLengthData        // Omit sequence, quality and auxiliary data.
)

// Read returns the next sam.Record in the BAM stream.
//
// The sam.Record returned will not contain the sequence, quality or
// auxiliary tag data if Omit(AllVariableLengthData) has been called
// prior to the Read call and will not contain the auxiliary tag data
// is Omit(AuxTags) has been called.
func (br *Reader) Read() (*sam.Record, error) {
	if br.c != nil && vOffset(br.r.LastChunk().End) >= vOffset(br.c.End) {
		return nil, io.EOF
	}
	// Use a pool of buffer's to share buffers between concurrent clients
	// and hence reduce the number of allocations required.
	buf := bufPool.Get().([]byte)
	if err := readAlignment(br, &buf); err != nil {
		bufPool.Put(buf)
		return nil, err
	}
	rec, err := unmarshal(buf, br.h, br.omit)
	bufPool.Put(buf)
	return rec, err
}

// Unmarshal a serialized record.  Parameter omit is the value of Reader.Omit().
// Most callers should pass zero as omit.
func unmarshal(b []byte, header *sam.Header, omit int) (*sam.Record, error) {
	rec := sam.GetFromFreePool()
	if len(b) < 32 {
		return nil, errors.New("bam: record too short")
	}
	// Need to use int(int32(uint32)) to ensure 2's complement extension of -1.
	refID := int(int32(binary.LittleEndian.Uint32(b)))
	rec.Pos = int(int32(binary.LittleEndian.Uint32(b[4:])))
	nLen := int(b[8])
	rec.MapQ = b[9]
	nCigar := int(binary.LittleEndian.Uint16(b[12:]))
	rec.Flags = sam.Flags(binary.LittleEndian.Uint16(b[14:]))
	lSeq := int(binary.LittleEndian.Uint32(b[16:]))
	nextRefID := int(int32(binary.LittleEndian.Uint32(b[20:])))
	rec.MatePos = int(int32(binary.LittleEndian.Uint32(b[24:])))
	rec.TempLen = int(int32(binary.LittleEndian.Uint32(b[28:])))

	// Read variable length data.
	pos := 32

	blen := len(b) - pos
	cigarOffset := alignOffset(blen)                     // store the cigar int32s here
	auxOffset := alignOffset(cigarOffset + (nCigar * 4)) // store the AuxFields here

	nDoubletBytes := (lSeq + 1) >> 1
	bAuxOffset := pos + nLen + (nCigar * 4) + nDoubletBytes + lSeq
	if len(b) < bAuxOffset {
		return nil, fmt.Errorf("Corrupt BAM aux record: len(b)=%d, auxoffset=%d", len(b), bAuxOffset)
	}
	nAuxFields, err := countAuxFields(b[bAuxOffset:])
	if err != nil {
		return nil, err
	}
	shadowSize := auxOffset + (nAuxFields * sizeofSliceHeader)

	// shadowBuf is used as an 'arena' from which all objects/slices
	// required to store the result of parsing the bam alignment record.
	// This reduces the load on GC and consequently allows for better
	// scalability with the number of cores used by clients of this package.
	shadowOffset := 0
	resizeScratch(&rec.Scratch, shadowSize)
	shadowBuf := rec.Scratch
	copy(shadowBuf, b[pos:])

	bufHdr := (*reflect.SliceHeader)(unsafe.Pointer(&shadowBuf))

	// Note that rec.Name now points to the shadow buffer
	hdr := (*reflect.StringHeader)(unsafe.Pointer(&rec.Name))
	hdr.Data = uintptr(unsafe.Pointer(bufHdr.Data))
	hdr.Len = nLen - 1 // drop trailing '\0'
	shadowOffset += nLen

	var sliceHdr *reflect.SliceHeader

	if nCigar > 0 {
		for i := 0; i < nCigar; i++ {
			*(*uint32)(unsafe.Pointer(&shadowBuf[cigarOffset+(i*4)])) = binary.LittleEndian.Uint32(shadowBuf[shadowOffset+(i*4):])
		}
		sliceHdr = (*reflect.SliceHeader)(unsafe.Pointer(&rec.Cigar))
		sliceHdr.Data = bufHdr.Data + uintptr(cigarOffset)
		sliceHdr.Len = nCigar
		sliceHdr.Cap = sliceHdr.Len
		shadowOffset += nCigar * 4
	} else {
		sliceHdr = (*reflect.SliceHeader)(unsafe.Pointer(&rec.Cigar))
		sliceHdr.Data = uintptr(0)
		sliceHdr.Len = 0
		sliceHdr.Cap = 0
	}

	if omit >= AllVariableLengthData {
		goto done
	}

	rec.Seq.Length = lSeq

	sliceHdr = (*reflect.SliceHeader)(unsafe.Pointer(&rec.Seq.Seq))
	sliceHdr.Data = uintptr(unsafe.Pointer(bufHdr.Data + uintptr(shadowOffset)))
	sliceHdr.Len = nDoubletBytes
	sliceHdr.Cap = sliceHdr.Len
	shadowOffset += nDoubletBytes

	if omit >= AuxTags {
		goto done
	}

	sliceHdr = (*reflect.SliceHeader)(unsafe.Pointer(&rec.Qual))
	sliceHdr.Data = uintptr(unsafe.Pointer(bufHdr.Data + uintptr(shadowOffset)))
	sliceHdr.Len = lSeq
	sliceHdr.Cap = sliceHdr.Len

	shadowOffset += lSeq

	if nAuxFields > 0 {
		// Clear the array before updating rec.AuxFields. GC will be
		// confused otherwise.
		for i := auxOffset; i < auxOffset+nAuxFields*sizeofSliceHeader; i++ {
			shadowBuf[i] = 0
		}
		sliceHdr = (*reflect.SliceHeader)(unsafe.Pointer(&rec.AuxFields))
		sliceHdr.Data = uintptr(unsafe.Pointer(bufHdr.Data + uintptr(auxOffset)))
		sliceHdr.Len = nAuxFields
		sliceHdr.Cap = sliceHdr.Len
		parseAux(shadowBuf[shadowOffset:blen], rec.AuxFields)
	}

done:
	refs := len(header.Refs())
	if refID != -1 {
		if refID < -1 || refID >= refs {
			return nil, errors.New("bam: reference id out of range")
		}
		rec.Ref = header.Refs()[refID]
	}
	if nextRefID != -1 {
		if refID == nextRefID {
			rec.MateRef = rec.Ref
			return rec, nil
		}
		if nextRefID < -1 || nextRefID >= refs {
			return nil, errors.New("bam: mate reference id out of range")
		}
		rec.MateRef = header.Refs()[nextRefID]
	}
	return rec, nil
}

// SetCache sets the cache to be used by the Reader.
func (bg *Reader) SetCache(c bgzf.Cache) {
	bg.r.SetCache(c)
}

// Seek performs a seek to the specified bgzf.Offset.
func (br *Reader) Seek(off bgzf.Offset) error {
	return br.r.Seek(off)
}

// SetChunk sets a limited range of the underlying BGZF file to read, after
// seeking to the start of the given chunk. It may be used to iterate over
// a defined genomic interval.
func (br *Reader) SetChunk(c *bgzf.Chunk) error {
	if c != nil {
		err := br.r.Seek(c.Begin)
		if err != nil {
			return err
		}
	}
	br.c = c
	return nil
}

// LastChunk returns the bgzf.Chunk corresponding to the last Read operation.
// The bgzf.Chunk returned is only valid if the last Read operation returned a
// nil error.
func (br *Reader) LastChunk() bgzf.Chunk {
	return br.lastChunk
}

// Close closes the Reader.
func (br *Reader) Close() error {
	return br.r.Close()
}

// Iterator wraps a Reader to provide a convenient loop interface for reading BAM data.
// Successive calls to the Next method will step through the features of the provided
// Reader. Iteration stops unrecoverably at EOF or the first error.
type Iterator struct {
	r *Reader

	chunks []bgzf.Chunk

	rec *sam.Record
	err error
}

// NewIterator returns a Iterator to read from r, limiting the reads to the provided
// chunks.
//
//  chunks, err := idx.Chunks(ref, beg, end)
//  if err != nil {
//  	return err
//  }
//  i, err := NewIterator(r, chunks)
//  if err != nil {
//  	return err
//  }
//  for i.Next() {
//  	fn(i.Record())
//  }
//  return i.Close()
//
func NewIterator(r *Reader, chunks []bgzf.Chunk) (*Iterator, error) {
	if len(chunks) == 0 {
		return &Iterator{r: r, err: io.EOF}, nil
	}
	err := r.SetChunk(&chunks[0])
	if err != nil {
		return nil, err
	}
	chunks = chunks[1:]
	return &Iterator{r: r, chunks: chunks}, nil
}

// Next advances the Iterator past the next record, which will then be available through
// the Record method. It returns false when the iteration stops, either by reaching the end of the
// input or an error. After Next returns false, the Error method will return any error that
// occurred during iteration, except that if it was io.EOF, Error will return nil.
func (i *Iterator) Next() bool {
	if i.err != nil {
		return false
	}
	i.rec, i.err = i.r.Read()
	if len(i.chunks) != 0 && i.err == io.EOF {
		i.err = i.r.SetChunk(&i.chunks[0])
		i.chunks = i.chunks[1:]
		return i.Next()
	}
	return i.err == nil
}

// Error returns the first non-EOF error that was encountered by the Iterator.
func (i *Iterator) Error() error {
	if i.err == io.EOF {
		return nil
	}
	return i.err
}

// Record returns the most recent record read by a call to Next.
func (i *Iterator) Record() *sam.Record { return i.rec }

// Close releases the underlying Reader.
func (i *Iterator) Close() error {
	i.r.SetChunk(nil)
	return i.Error()
}

var jumps = [256]int{
	'A': 1,
	'c': 1, 'C': 1,
	's': 2, 'S': 2,
	'i': 4, 'I': 4,
	'f': 4,
	'Z': -1,
	'H': -1,
	'B': -1,
}

var errCorruptAuxField = errors.New("Corrupt aux field")

// countAuxFields examines the data of a SAM record's OPT field to determine
// the number of auxFields there are.
func countAuxFields(aux []byte) (int, error) {
	naux := 0
	for i := 0; i+2 < len(aux); {
		t := aux[i+2]
		switch j := jumps[t]; {
		case j > 0:
			j += 3
			i += j
			naux++
		case j < 0:
			switch t {
			case 'Z', 'H':
				var (
					j int
					v byte
				)
				for j, v = range aux[i:] {
					if v == 0 { // C string termination
						break // Truncate terminal zero.
					}
				}
				i += j + 1
				naux++
			case 'B':
				if len(aux) < i+8 {
					return -1, errCorruptAuxField
				}
				length := binary.LittleEndian.Uint32(aux[i+4 : i+8])
				j = int(length)*jumps[aux[i+3]] + int(unsafe.Sizeof(length)) + 4
				i += j
				naux++
			}
		default:
			return -1, errCorruptAuxField
		}
	}
	return naux, nil
}

// parseAux examines the data of a SAM record's OPT fields,
// returning a slice of sam.Aux that are backed by the original data.
func parseAux(aux []byte, aa []sam.Aux) {
	naa := 0
	/*	var sliceHdr *reflect.SliceHeader
		auxSlice := (*reflect.SliceHeader)(unsafe.Pointer(&aux))*/
	for i := 0; i+2 < len(aux); {
		t := aux[i+2]
		switch j := jumps[t]; {
		case j > 0:
			j += 3
			aa[naa] = sam.Aux(aux[i : i+j : i+j])
			naa++
			i += j
		case j < 0:
			switch t {
			case 'Z', 'H':
				var (
					j int
					v byte
				)
				for j, v = range aux[i:] {
					if v == 0 { // C string termination
						break // Truncate terminal zero.
					}
				}
				aa[naa] = sam.Aux(aux[i : i+j : i+j])
				naa++
				i += j + 1
			case 'B':
				length := binary.LittleEndian.Uint32(aux[i+4 : i+8])
				j = int(length)*jumps[aux[i+3]] + int(unsafe.Sizeof(length)) + 4
				aa[naa] = sam.Aux(aux[i : i+j : i+j])
				naa++
				i += j
			}
		default:
			panic(fmt.Sprintf("bam: unrecognised optional field type: %q", t))
		}
	}
}

// readAlignment reads the alignment record from the Reader's underlying
// bgzf.Reader into the supplied bytes.Buffer and updates the Reader's lastChunk
// field.
func readAlignment(br *Reader, buf *[]byte) error {
	n, err := io.ReadFull(br.r, br.sizeBuf)
	// br.r.Chunk() is only valid after the call the Read(), so this
	// must come after the first read in the record.
	tx := br.r.Begin()
	defer func() {
		br.lastChunk = tx.End()
	}()
	if err != nil {
		return err
	}
	if n != 4 {
		return errors.New("bam: invalid record: short block size")
	}
	size := int(binary.LittleEndian.Uint32(br.sizeBuf))
	if size > maxBAMRecordSize {
		return errors.New("bam: record too large")
	}
	resizeScratch(buf, size)
	nn, err := io.ReadFull(br.r, *buf)
	if err != nil {
		return err
	}
	if nn != size {
		return errors.New("bam: truncated record")
	}
	return nil
}

// buildAux constructs a single byte slice that represents a slice of sam.Aux.
// *buf should be an empty slice on call, and it is filled with the result on
// return.
func buildAux(aa []sam.Aux, buf *[]byte) {
	for _, a := range aa {
		// TODO: validate each 'a'
		*buf = append(*buf, []byte(a)...)
		switch a.Type() {
		case 'Z', 'H':
			*buf = append(*buf, 0)
		}
	}
}

type doublets []sam.Doublet

func (np doublets) Bytes() []byte { return *(*[]byte)(unsafe.Pointer(&np)) }
