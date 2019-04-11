package sam

import (
	"testing"

	"github.com/grailbio/testutil/assert"
)

var (
	diTag = Tag{'D', 'I'}
	dsTag = Tag{'D', 'S'}
)

func TestGetUnique(t *testing.T) {
	r := GetFromFreePool()
	// Case 1: No Aux fields.  Return should be nil, nil.
	r.AuxFields = AuxFields{}
	tag, err := r.AuxFields.GetUnique(diTag)
	assert.NoError(t, err)
	assert.Nil(t, tag)

	// Case 2: Tag appears once.
	var newAux Aux
	newAux, err = NewAux(diTag, "1")
	assert.NoError(t, err)
	r.AuxFields = append(r.AuxFields, newAux)
	newAux, err = NewAux(dsTag, 2)
	assert.NoError(t, err)
	r.AuxFields = append(r.AuxFields, newAux)

	tag, err = r.AuxFields.GetUnique(diTag)
	assert.NoError(t, err)
	assert.NotNil(t, tag)

	// Case 3: Tag appears multiple times.
	newAux, err = NewAux(diTag, "3")
	assert.NoError(t, err)
	r.AuxFields = append(r.AuxFields, newAux)
	newAux, err = NewAux(dsTag, 4)
	assert.NoError(t, err)
	r.AuxFields = append(r.AuxFields, newAux)

	tag, err = r.AuxFields.GetUnique(diTag)
	assert.NotNil(t, err)
}
