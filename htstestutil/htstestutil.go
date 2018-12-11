package htstestutil

import (
	"sync"

	"github.com/grailbio/hts/sam"
	"github.com/grailbio/testutil/h"
)

var once = sync.Once{}

// RegisterSAMRecordComparator adds a github.com/grailbio/testutil/h comparator
// for sam.Record. This function is threadsafe & idempotent.
func RegisterSAMRecordComparator() {
	once.Do(func() {
		h.RegisterComparator(func(f0, f1 sam.Record) (int, error) {
			if f0.Equal(&f1) {
				return 0, nil
			}
			return 1, nil
		})
	})
}
