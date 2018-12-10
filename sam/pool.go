//go:generate  ../../base/gtl/generate_randomized_freepool.py --output=record_pool --prefix=Record -DELEM=*RecordWithScratchBuf --package=sam

package sam

import (
	"sync/atomic"
	"unsafe"

	"v.io/x/lib/vlog"
) // Must be the same as grailbam.Magic
const Magic = uint64(0x93c9838d4d9f4f71)

// RecordWithScratchBuf is exactly the same as
// grail.com/bio/encoding/bam.Record.  It's copied here to avoid cyclic
// dependency.
type RecordWithScratchBuf struct {
	Record
	Magic   uint64
	Scratch []byte
}

var recordPool = NewRecordFreePool(func() *RecordWithScratchBuf { return &RecordWithScratchBuf{Magic: Magic} }, 1<<20)
var nPoolWarnings int32

// CastToRecord casts *RecordWithScratchBuf to *Record.
func CastToRecord(rb *RecordWithScratchBuf) *Record {
	return (*Record)(unsafe.Pointer(rb))
}

// GetFromFreePool allocates a new empty Record object.  It is set by
// grail.com/bio/encoding/bam when the process boots.
func GetFromFreePool() *Record {
	rec := recordPool.Get()
	rec.Name = ""
	rec.Ref = nil
	rec.MateRef = nil
	rec.Cigar = nil
	rec.Seq = Seq{}
	rec.Qual = nil
	rec.AuxFields = nil
	return (*Record)(unsafe.Pointer(rec))
}

// PutInFreePool adds "r" to the singleton freepool.  The caller must guarantee
// that there is no outstanding references to "r"; "r" will be overwritten in a
// future.
func PutInFreePool(samr *Record) {
	r := (*RecordWithScratchBuf)(unsafe.Pointer(samr))
	if r == nil {
		panic("r=nil")
	}
	if r.Magic != Magic {
		if atomic.AddInt32(&nPoolWarnings, 1) < 2 {
			vlog.Errorf(`putSamRecord: object must be bam.Record, not sam.Record. magic %x.
If you see this warning in non-test code path, you MUST fix the problem`, r.Magic)
		}
		return
	}
	recordPool.Put(r)
}
