//go:generate  ../../base/gtl/generate_randomized_freepool.py --output=record_pool --prefix=Record -DELEM=*Record --package=sam

package sam

// This file contains Grail-specific extensions.

import (
	"bytes"

	gunsafe "github.com/grailbio/base/unsafe"
)

var recordPool    = NewRecordFreePool(func() *Record { return &Record{} }, 1<<20)

// ResizeScratch makes *buf exactly n bytes long.
func ResizeScratch(buf *[]byte, n int) {
	if cap(*buf) < n {
		// Allocate slightly more memory than needed to prevent frequent
		// reallocation.
		size := (n/16 + 1) * 16
		*buf = make([]byte, n, size)
	} else {
		gunsafe.ExtendBytes(buf, n)
	}
}

// GetFromFreePool allocates a new empty Record object.
func GetFromFreePool() *Record {
	rec := recordPool.Get()
	rec.Name = ""
	rec.Ref = nil
	rec.MateRef = nil
	rec.Cigar = nil
	rec.Seq = Seq{}
	rec.Qual = nil
	rec.AuxFields = nil
	return rec
}

// PutInFreePool adds the record to the singleton freepool.  The caller must
// guarantee that there is no outstanding references to the record. It will be
// overwritten in a future.
func PutInFreePool(r *Record) {
	recordPool.Put(r)
}

// Equal checks if the two records are identical, except for the Scratch field.
func (r *Record) Equal(other *Record) bool {
	return r.Name == other.Name &&
		r.Ref == other.Ref &&
		r.Pos == other.Pos &&
		r.MapQ == other.MapQ &&
		r.Cigar.Equal(other.Cigar) &&
		r.Flags == other.Flags &&
		r.MateRef == other.MateRef &&
		r.MatePos == other.MatePos &&
		r.TempLen == other.TempLen &&
		r.Seq.Equal(other.Seq) &&
		bytes.Equal(r.Qual, other.Qual) &&
		r.AuxFields.Equal(other.AuxFields)
}

// Equal checks if the two values are identical.
func (s Seq) Equal(other Seq) bool {
	// TODO(satio) Use UnsafeDoubletsToBytes
	if s.Length != other.Length {
		return false
	}
	for i := range s.Seq {
		if s.Seq[i] != other.Seq[i] {
			return false
		}
	}
	return true
}

// Equal checks if the two values are identical.
func (s Cigar) Equal(other Cigar) bool {
	if len(s) != len(other) {
		return false
	}
	for i := range s {
		if s[i] != other[i] {
			return false
		}
	}
	return true
}

// Equal checks if the two values are identical.
func (s AuxFields) Equal(other AuxFields) bool {
	if len(s) != len(other) {
		return false
	}
	for i := range s {
		if !bytes.Equal(s[i], other[i]) {
			return false
		}
	}
	return true
}
