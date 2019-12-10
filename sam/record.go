// Copyright ©2012-2013 The bíogo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sam

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"unsafe"

	"github.com/grailbio/base/simd"
	"github.com/grailbio/bio/biosimd"
	"github.com/grailbio/hts/internal"
)

// DupType enumerates the different possible values for the Duplicate
// Type (DT) aux tag.
type DupType int

const (
	// DupTypeNone specifies duplicate type not present.
	DupTypeNone DupType = iota

	// DupTypeLB specifies "library" or PCR duplicate type.
	DupTypeLB

	// DupTypeSQ specifies "sequencer" or optical duplicate type.
	DupTypeSQ
)

// LinearDupState enumerates the different possible Linear Duplicate
// states for a read pair, stored in the LD aux tag.
type LinearDupState int

const (
	// LinearNone specifies linear dup tag not present.
	LinearNone = iota

	// LinearPrimary specifies linear primary.
	LinearPrimary

	// LinearDuplicate specifies linear duplicate.
	LinearDuplicate
)

// Record represents a SAM/BAM record.
type Record struct {
	Name      string
	Ref       *Reference
	Pos       int
	MapQ      byte
	Cigar     Cigar
	Flags     Flags
	MateRef   *Reference
	MatePos   int
	TempLen   int
	Seq       Seq
	Qual      []byte
	AuxFields AuxFields

	Scratch []byte
}

// NewRecord returns a Record, checking for consistency of the provided
// attributes.
func NewRecord(name string, ref, mRef *Reference, p, mPos, tLen int, mapQ byte, co []CigarOp, seq, qual []byte, aux []Aux) (*Record, error) {
	if !(validPos(p) && validPos(mPos) && validTmpltLen(tLen) && validLen(len(seq)) && (qual == nil || validLen(len(qual)))) {
		return nil, errors.New("sam: value out of range")
	}
	if len(name) == 0 || len(name) > 254 {
		return nil, errors.New("sam: name absent or too long")
	}
	if qual != nil && len(qual) != len(seq) {
		return nil, errors.New("sam: sequence/quality length mismatch")
	}
	if ref != nil {
		if ref.id < 0 {
			return nil, errors.New("sam: linking to invalid reference")
		}
	} else {
		if p != -1 {
			return nil, errors.New("sam: specified position != -1 without reference")
		}
	}
	if mRef != nil {
		if mRef.id < 0 {
			return nil, errors.New("sam: linking to invalid mate reference")
		}
	} else {
		if mPos != -1 {
			return nil, errors.New("sam: specified mate position != -1 without mate reference")
		}
	}
	r := GetFromFreePool()
	r.Name = name
	r.Ref = ref
	r.Pos = p
	r.MapQ = mapQ
	r.Cigar = co
	r.Flags = 0
	r.MateRef = mRef
	r.MatePos = mPos
	r.TempLen = tLen
	r.Seq = NewSeq(seq)
	r.Qual = qual
	r.AuxFields = aux
	return r, nil
}

// IsValidRecord returns whether the record satisfies the conditions that
// it has the Unmapped flag set if it not placed; that the MateUnmapped
// flag is set if it paired its mate is unplaced; that the CIGAR length
// matches the sequence and quality string lengths if they are non-zero; and
// that the Paired, ProperPair, Unmapped and MateUnmapped flags are consistent.
func IsValidRecord(r *Record) bool {
	if (r.Ref == nil || r.Pos == -1) && r.Flags&Unmapped == 0 {
		return false
	}
	if r.Flags&Paired != 0 && (r.MateRef == nil || r.MatePos == -1) && r.Flags&MateUnmapped == 0 {
		return false
	}
	if r.Flags&(Unmapped|ProperPair) == Unmapped|ProperPair {
		return false
	}
	if r.Flags&(Paired|MateUnmapped|ProperPair) == Paired|MateUnmapped|ProperPair {
		return false
	}
	if len(r.Qual) != 0 && r.Seq.Length != len(r.Qual) {
		return false
	}
	if cigarLen := r.Len(); cigarLen < 0 || (r.Seq.Length != 0 && r.Seq.Length != cigarLen) {
		return false
	}
	return true
}

// Tag returns an Aux tag whose tag ID matches the first two bytes of tag and true.
// If no tag matches, nil and false are returned.
func (r *Record) Tag(tag []byte) (v Aux, ok bool) {
	if len(tag) < 2 {
		panic("sam: tag too short")
	}
	for _, aux := range r.AuxFields {
		if aux.matches(tag) {
			return aux, true
		}
	}
	return nil, false
}

// RefID returns the reference ID for the Record.
func (r *Record) RefID() int {
	return r.Ref.ID()
}

// Start returns the lower-coordinate end of the alignment.
func (r *Record) Start() int {
	return r.Pos
}

// Bin returns the BAM index bin of the record.
func (r *Record) Bin() int {
	if r.Flags&(Unmapped|MateUnmapped) == Unmapped|MateUnmapped {
		return 4680 // reg2bin(-1, 0)
	}
	end := r.End()

	// If the alignment length is zero (for example, if the read is
	// unmapped), then increment end by 1 and treat the read as length
	// 1 for binning purposes.
	if end == r.Pos {
		end++
	}

	if !internal.IsValidIndexPos(r.Pos) || !internal.IsValidIndexPos(end) {
		return -1
	}
	return int(internal.BinFor(r.Pos, end))
}

// Len returns the length of the alignment.
func (r *Record) Len() int {
	return r.End() - r.Start()
}

func max(a, b int) int {
	if a < b {
		return b
	}
	return a
}

// End returns the highest query-consuming coordinate end of the alignment.
// The position returned by End is not valid if r.Cigar.IsValid(r.Seq.Length)
// is false.
func (r *Record) End() int {
	if r.Flags&Unmapped != 0 || len(r.Cigar) == 0 {
		return r.Pos + 1
	}
	pos := r.Pos
	end := pos
	for _, co := range r.Cigar {
		pos += co.Len() * co.Type().Consumes().Reference
		end = max(end, pos)
	}
	return end
}

// Strand returns an int8 indicating the strand of the alignment. A positive return indicates
// alignment in the forward orientation, a negative returns indicates alignment in the reverse
// orientation.
func (r *Record) Strand() int8 {
	if r.Flags&Reverse == Reverse {
		return -1
	}
	return 1
}

// LessByName returns true if the receiver sorts by record name before other.
func (r *Record) LessByName(other *Record) bool {
	return r.Name < other.Name
}

// LessByCoordinate returns true if the receiver sorts by coordinate before other
// according to the SAM specification.
func (r *Record) LessByCoordinate(other *Record) bool {
	rRefName := r.Ref.Name()
	oRefName := other.Ref.Name()
	switch {
	case oRefName == "*":
		return true
	case rRefName == "*":
		return false
	}
	return (rRefName < oRefName) || (rRefName == oRefName && r.Pos < other.Pos)
}

// BagID returns the bag id (given by aux tag "DI") for r. If the DI
// tag is not set, returns (-1, nil). If the tag is present, but malformed,
// returns (-1, err).
func (r *Record) BagID() (int64, error) {
	val, found, err := r.auxInt64Value(bagIDTag)
	if found && val < 0 {
		return -1, fmt.Errorf("bag id: expected bag id >= 0, not %d", val)
	}
	return val, err
}

// BagSize returns the size of the bag as defined in the "DS" aux
// tag. For a description of the DS tag, please see
// bio-mark-duplicates. If the aux tag is not present, returns (-1,
// nil). If the aux tag is malformed, returns (-1, err).
func (r *Record) BagSize() (int, error) {
	val, found, err := r.auxIntValue(bagSizeTag)
	if found && val <= 0 {
		return -1, fmt.Errorf("bag size: expected bag size >= 1, not %d", val)
	}
	return val, err
}

// DupType returns (DupTypeSQ, nil) if r has the DT tag, and its value
// is "SQ" (optical). If the DT tag is present, and its value is "LB"
// (PCR), then returns (DupTypeLB, nil). If the DT tag is not present,
// then returns (DupTypeNone, nil). If the aux value is malformed,
// then returns (DupTypeNone, err).
func (r *Record) DupType() (DupType, error) {
	aux, err := r.AuxFields.GetUnique(dupTypeTag)
	if err != nil || aux == nil {
		return DupTypeNone, err
	}

	s, ok := aux.Value().(string)
	if !ok {
		return DupTypeNone, fmt.Errorf("optical dup: unexpected type: %s", aux.String())
	}

	switch s {
	case "SQ":
		return DupTypeSQ, nil
	case "LB":
		return DupTypeLB, nil
	}
	return DupTypeNone, fmt.Errorf("optical dup: unexpected value: %s", aux.String())
}

// LibraryBagSize returns the number of library duplicate fragments in the bag of the given
// record, as defined by the DL tag. For a description of the DL tag and how it relates to
// the DS tag, please see bio-mark-duplicates. If the DL tag is not present (e.g., earlier
// pipeline versions or read pairs with an unmapped read), -1 will be returned without an
// error. If the DL tag is malformed, an error will be returned.
func (r *Record) LibraryBagSize() (int, error) {
	val, found, err := r.auxIntValue(libraryBagSizeTag)
	if found && val < 1 {
		return -1, fmt.Errorf("%s: expected value >= 1, not %d", libraryBagSizeTag, val)
	}
	return val, err
}

// LinearDup returns the linear duplicate state of the record as
// specified in the LD aux tag. If the LD tag is not present (e.g.,
// earlier pipeline versions or read pairs with an unmapped read, or
// mapq below threshold), returns (LinearNone, nil). If the LD
// tag is present, but has an invalid value, then returns
// (LinearNone, err). Otherwise, returns (LinearPrimary,
// nil) or (LinearDuplicate, nil) depending on the value of the
// LD tag.
func (r *Record) LinearDup() (LinearDupState, error) {
	aux, err := r.AuxFields.GetUnique(linearDupTag)
	if err != nil || aux == nil {
		return LinearNone, err
	}

	s, ok := aux.Value().(string)
	if !ok {
		return LinearNone, fmt.Errorf("linear dup: unexpected type: %s", aux.String())
	}
	switch s {
	case "primary":
		return LinearPrimary, nil
	case "duplicate":
		return LinearDuplicate, nil
	}
	return LinearNone, fmt.Errorf("linear dup: unexpected value: %s", aux.String())
}

// LinearBagID returns the linear bag id (given by aux tag "LI") for r. If the LI
// tag is not set, returns (-1, nil). If the tag is present, but malformed,
// returns (-1, err).
func (r *Record) LinearBagID() (int64, error) {
	val, found, err := r.auxInt64Value(linearBagIDTag)
	if found && val < 0 {
		return -1, fmt.Errorf("linear bag id: expected bag id >= 0, not %d", val)
	}
	return val, err
}

// LinearBagSize returns the size of the linear bag as defined in the "LS" aux
// tag. If the aux tag is not present, returns (-1, nil). If the aux
// tag is malformed, returns (-1, err).
func (r *Record) LinearBagSize() (int, error) {
	val, found, err := r.auxIntValue(linearBagSizeTag)
	if found && val <= 0 {
		return -1, fmt.Errorf("linear bag size: expected bag size >= 1, not %d", val)
	}
	return val, err
}

// auxIntValue finds the unique specified aux tag. If there is an
// error while looking for the aux tag or the type is not an int,
// return (-1, false, err). If the aux tag is not present, return (-1,
// false, nil). If the aux tag is found, and it is an integer type,
// then return (value, true, nil).
func (r *Record) auxIntValue(tag Tag) (val int, found bool, err error) {
	aux, err := r.AuxFields.GetUnique(tag)
	if err != nil || aux == nil {
		return -1, false, err
	}

	switch v := aux.Value().(type) {
	case uint8:
		val = int(v)
	case int8:
		val = int(v)
	case int16:
		val = int(v)
	case uint16:
		val = int(v)
	case int32:
		val = int(v)
	default:
		return -1, false, fmt.Errorf("%s: unexpected type: %T", tag, v)
	}
	return val, true, nil
}

// auxInt64Value finds the unique specified aux tag. It is like
// auxIntValue, but returns an int64 and also converts a string type
// aux tag if it can be parsed as an integer.
//
// If there is an error while looking for the aux tag or the type is
// not an int, return (-1, false, err). If the aux tag is not present,
// return (-1, false, nil). If the aux tag is found, and it is an
// integer type, then return (value, true, nil).
func (r *Record) auxInt64Value(tag Tag) (val int64, found bool, err error) {
	aux, err := r.AuxFields.GetUnique(tag)
	if err != nil || aux == nil {
		return -1, false, err
	}

	switch v := aux.Value().(type) {
	case uint8:
		val = int64(v)
	case int8:
		val = int64(v)
	case int16:
		val = int64(v)
	case uint16:
		val = int64(v)
	case int32:
		val = int64(v)
	case string:
		val, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			return -1, false, err
		}
	default:
		return -1, false, fmt.Errorf("%s: unexpected type: %T", tag, v)
	}
	return val, true, nil
}

// String returns a string representation of the Record.
func (r *Record) String() string {
	end := r.End()
	return fmt.Sprintf("%s %v %v %d %s:%d..%d (%d) %d %s:%d %d %s %v %v",
		r.Name,
		r.Flags,
		r.Cigar,
		r.MapQ,
		r.Ref.Name(),
		r.Pos,
		end,
		r.Bin(),
		end-r.Pos,
		r.MateRef.Name(),
		r.MatePos,
		r.TempLen,
		r.Seq.Expand(),
		r.Qual,
		r.AuxFields,
	)
}

// UnmarshalText implements the encoding.TextUnmarshaler. It calls UnmarshalSAM with
// a nil Header.
func (r *Record) UnmarshalText(b []byte) error {
	return r.UnmarshalSAM(nil, b)
}

// UnmarshalSAM parses a SAM format alignment line in the provided []byte, using
// references from the provided Header. If a nil Header is passed to UnmarshalSAM
// and the SAM data include non-empty refence and mate reference names, fake
// references with zero length and an ID of -1 are created to hold the reference
// names.
func (r *Record) UnmarshalSAM(h *Header, b []byte) error {
	f := bytes.Split(b, []byte{'\t'})
	if len(f) < 11 {
		return errors.New("sam: missing SAM fields")
	}
	*r = Record{Name: string(f[0])}
	// TODO(kortschak): Consider parsing string format flags.
	flags, err := strconv.ParseUint(string(f[1]), 0, 16)
	if err != nil {
		return fmt.Errorf("sam: failed to parse flags: %v", err)
	}
	r.Flags = Flags(flags)
	r.Ref, err = referenceForName(h, string(f[2]))
	if err != nil {
		return fmt.Errorf("sam: failed to assign reference: %v", err)
	}
	r.Pos, err = strconv.Atoi(string(f[3]))
	r.Pos--
	if err != nil {
		return fmt.Errorf("sam: failed to parse position: %v", err)
	}
	mapQ, err := strconv.ParseUint(string(f[4]), 10, 8)
	if err != nil {
		return fmt.Errorf("sam: failed to parse map quality: %v", err)
	}
	r.MapQ = byte(mapQ)
	r.Cigar, err = ParseCigar(f[5])
	if err != nil {
		return fmt.Errorf("sam: failed to parse cigar string: %v", err)
	}
	if bytes.Equal(f[2], f[6]) || bytes.Equal(f[6], []byte{'='}) {
		r.MateRef = r.Ref
	} else {
		r.MateRef, err = referenceForName(h, string(f[6]))
		if err != nil {
			return fmt.Errorf("sam: failed to assign mate reference: %v", err)
		}
	}
	r.MatePos, err = strconv.Atoi(string(f[7]))
	r.MatePos--
	if err != nil {
		return fmt.Errorf("sam: failed to parse mate position: %v", err)
	}
	r.TempLen, err = strconv.Atoi(string(f[8]))
	if err != nil {
		return fmt.Errorf("sam: failed to parse template length: %v", err)
	}
	if !bytes.Equal(f[9], []byte{'*'}) {
		r.Seq = NewSeq(f[9])
		if len(r.Cigar) != 0 && !r.Cigar.IsValid(r.Seq.Length) {
			return errors.New("sam: sequence/CIGAR length mismatch")
		}
	}
	if !bytes.Equal(f[10], []byte{'*'}) {
		r.Qual = append(r.Qual, f[10]...)
		simd.AddConst8Inplace(r.Qual, 256-33)
	} else if r.Seq.Length != 0 {
		r.Qual = make([]byte, r.Seq.Length)
		simd.Memset8(r.Qual, 0xff)
	}
	if len(r.Qual) != 0 && len(r.Qual) != r.Seq.Length {
		return errors.New("sam: sequence/quality length mismatch")
	}
	if len(f) > 11 {
		r.AuxFields = make([]Aux, len(f)-11)
		for i, aux := range f[11:] {
			a, err := ParseAux(aux)
			if err != nil {
				return err
			}
			r.AuxFields[i] = a
		}
	}
	return nil
}

func referenceForName(h *Header, name string) (*Reference, error) {
	if name == "*" {
		return nil, nil
	}
	if h == nil {
		// If we don't have a Header, return a fake Reference.
		return &Reference{
			id:   -1,
			name: name,
		}, nil
	}

	for _, r := range h.refs {
		if r.Name() == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no reference with name %q", name)
}

// MarshalText implements encoding.TextMarshaler. It calls MarshalSAM with FlagDecimal.
func (r *Record) MarshalText() ([]byte, error) {
	return r.MarshalSAM(0)
}

// MarshalSAM formats a Record as SAM using the specified flag format. Acceptable
// formats are FlagDecimal, FlagHex and FlagString.
func (r *Record) MarshalSAM(flags int) ([]byte, error) {
	if flags < FlagDecimal || flags > FlagString {
		return nil, errors.New("sam: flag format option out of range")
	}
	if r.Qual != nil && len(r.Qual) != r.Seq.Length {
		return nil, errors.New("sam: sequence/quality length mismatch")
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s\t%v\t%s\t%d\t%d\t%s\t%s\t%d\t%d\t%s\t%s",
		r.Name,
		formatFlags(r.Flags, flags),
		r.Ref.Name(),
		r.Pos+1,
		r.MapQ,
		r.Cigar,
		formatMate(r.Ref, r.MateRef),
		r.MatePos+1,
		r.TempLen,
		formatSeq(r.Seq),
		formatQual(r.Qual),
	)
	for _, t := range r.AuxFields {
		fmt.Fprintf(&buf, "\t%v", samAux(t))
	}
	return buf.Bytes(), nil
}

// Flag format constants.
const (
	FlagDecimal = iota
	FlagHex
	FlagString
)

func formatFlags(f Flags, format int) interface{} {
	switch format {
	case FlagDecimal:
		return uint16(f)
	case FlagHex:
		return fmt.Sprintf("0x%x", f)
	case FlagString:
		// If 0x01 is unset, no assumptions can be made about 0x02, 0x08, 0x20, 0x40 and 0x80
		const pairedMask = ProperPair | MateUnmapped | MateReverse | MateReverse | Read1 | Read2
		if f&1 == 0 {
			f &^= pairedMask
		}

		const flags = "pPuUrR12sfdS"

		b := make([]byte, 0, len(flags))
		for i, c := range flags {
			if f&(1<<uint(i)) != 0 {
				b = append(b, byte(c))
			}
		}

		return string(b)
	default:
		panic("sam: invalid flag format")
	}
}

func formatMate(ref, mate *Reference) string {
	if mate != nil && ref == mate {
		return "="
	}
	return mate.Name()
}

func formatSeq(s Seq) []byte {
	if s.Length == 0 {
		return []byte{'*'}
	}
	return s.Expand()
}

func formatQual(q []byte) []byte {
	for _, v := range q {
		if v != 0xff {
			a := make([]byte, len(q))
			simd.AddConst8(a, q, 33)
			return a
		}
	}
	return []byte{'*'}
}

// Doublet is a nybble-encode pair of nucleotide bases.
type Doublet byte

// Seq is a nybble-encode pair of nucleotide sequence.
type Seq struct {
	Length int
	Seq    []Doublet
}

var (
	n16TableRev = simd.MakeNibbleLookupTable([16]byte{'=', 'A', 'C', 'M', 'G', 'R', 'S', 'V', 'T', 'W', 'Y', 'H', 'K', 'D', 'B', 'N'})
	n16Table    = [256]Doublet{
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0x1, 0x2, 0x4, 0x8, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0x0, 0xf, 0xf,
		0xf, 0x1, 0xe, 0x2, 0xd, 0xf, 0xf, 0x4, 0xb, 0xf, 0xf, 0xc, 0xf, 0x3, 0xf, 0xf,
		0xf, 0xf, 0x5, 0x6, 0x8, 0xf, 0x7, 0x9, 0xf, 0xa, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0x1, 0xe, 0x2, 0xd, 0xf, 0xf, 0x4, 0xb, 0xf, 0xf, 0xc, 0xf, 0x3, 0xf, 0xf,
		0xf, 0xf, 0x5, 0x6, 0x8, 0xf, 0x7, 0x9, 0xf, 0xa, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
		0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf, 0xf,
	}
)

// NewSeq returns a new Seq based on the given byte slice.
func NewSeq(s []byte) Seq {
	return Seq{
		Length: len(s),
		Seq:    contract(s),
	}
}

func contract(s []byte) []Doublet {
	ns := make([]Doublet, (len(s)+1)>>1)
	var np Doublet
	for i, b := range s {
		if i&1 == 0 {
			np = n16Table[b] << 4
		} else {
			ns[i>>1] = np | n16Table[b]
		}
	}
	// We haven't written the last base if the
	// sequence was odd length, so do that now.
	if len(s)&1 != 0 {
		ns[len(ns)-1] = np
	}
	return ns
}

// Expand returns the byte encoded form of the receiver.
//
// This now has decent performance for ns.Length >= 32 (allocation is now the
// main bottleneck in that case), but it should still be avoided in new code.
// Base/BaseChar() is better if you are just performing a small number of point
// queries.  Direct calls to biosimd.UnpackSeq{Unsafe} or
// UnpackAndReplaceSeq{Unsafe}, which populate preallocated buffers, are better
// when you are iterating through many bases.  (The main advantage of the
// Unsafe functions is great performance for length < 32.)
func (ns Seq) Expand() []byte {
	s := make([]byte, ns.Length)
	nsSeqPtr := (*[]byte)(unsafe.Pointer(&ns.Seq))
	biosimd.UnpackAndReplaceSeq(s, *nsSeqPtr, &n16TableRev)
	return s
}

// SeqBase is BAM's 4-bit encoding of nucleotide base types. See section 4.2 of
// https://samtools.github.io/hts-specs/SAMv1.pdf
type SeqBase byte

const (
	// Commonly used SeqBase constants.
	BaseA SeqBase = 1
	BaseC SeqBase = 2
	BaseG SeqBase = 4
	BaseT SeqBase = 8
	BaseS SeqBase = 6
	BaseN SeqBase = 15

	// NumSeqBaseTypes is number of possible SeqBase values.  SeqBase starts
	// from 0.
	NumSeqBaseTypes = 16
)

func CharToSeqBase(char byte) SeqBase {
	return SeqBase(n16Table[char])
}

// Base returns the pos'th base of the sequence.
//
// REQUIRES: 0 <= pos < seq.Length
func (ns Seq) Base(pos int) SeqBase {
	var base SeqBase
	if pos%2 == 0 {
		base = SeqBase(ns.Seq[pos/2] >> 4)
	} else {
		base = SeqBase(ns.Seq[pos/2] & 0xf)
	}
	return base
}

// BaseChar returns the pos'th base of the as a character, such as 'A', 'T'.
//
// REQUIRES: 0 <= pos < seq.Length
func (ns Seq) BaseChar(pos int) byte {
	return n16TableRev.Get(byte(ns.Base(pos)))
}

// Char converts a SeqBase to a human-readable character.  For example,
// BaseA.Char() == 'A'.
//
// REQUIRES: 0 <= b < NumSeqBaseTypes
func (b SeqBase) Char() byte {
	return n16TableRev.Get(byte(b))
}
