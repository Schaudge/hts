package bam

import (
	"sync"
	"reflect"
	"unsafe"
)

var bufPool = sync.Pool{
	New: func() interface{} {
		return []byte{}
	},
}

func resizeScratch(buf *[]byte, n int) {
	if *buf == nil || cap(*buf) < n {
		// Allocate slightly more memory than needed to prevent frequent
		// reallocation.
		size := (n/16 + 1) * 16
		*buf = make([]byte, n, size)
	} else {
		*buf = (*buf)[:n]
	}
}

const sizeofSliceHeader = int(unsafe.Sizeof(reflect.SliceHeader{}))

// Round "off" up so that it is a multiple of 8. Used when storing a pointer in
// []byte.  8-byte alignment is sufficient for all CPUs we care about.
func alignOffset(off int) int {
	const pointerSize = 8
	return ((off-1)/pointerSize + 1) * pointerSize
}
