// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package reflect

import (
	"internal/abi"
	"unsafe"
)

// abiStep represents an ABI "instruction." Each instruction
// describes one part of how to translate between a Go value
// in memory and a call frame.
type abiStep struct {
	kind abiStepKind

	// offset and size together describe a part of a Go value
	// in memory.
	offset uintptr
	size   uintptr // size in bytes of the part

	// These fields describe the ABI side of the translation.
	stkOff uintptr // stack offset, used if kind == abiStepStack
	ireg   int     // integer register index, used if kind == abiStepIntReg or kind == abiStepPointer
	freg   int     // FP register index, used if kind == abiStepFloatReg
}

// abiStepKind is the "op-code" for an abiStep instruction.
type abiStepKind int

const (
	abiStepBad      abiStepKind = iota
	abiStepStack                // copy to/from stack
	abiStepIntReg               // copy to/from integer register
	abiStepPointer              // copy pointer to/from integer register
	abiStepFloatReg             // copy to/from FP register
)

// abiSeq represents a sequence of ABI instructions for copying
// from a series of reflect.Values to a call frame (for call arguments)
// or vice-versa (for call results).
//
// An abiSeq should be populated by calling its addArg method.
type abiSeq struct {
	// steps is the set of instructions.
	//
	// The instructions are grouped together by whole arguments,
	// with the starting index for the instructions
	// of the i'th Go value available in valueStart.
	//
	// For instance, if this abiSeq represents 3 arguments
	// passed to a function, then the 2nd argument's steps
	// begin at steps[valueStart[1]].
	//
	// Because reflect accepts Go arguments in distinct
	// Values and each Value is stored separately, each abiStep
	// that begins a new argument will have its offset
	// field == 0.
	steps      []abiStep
	valueStart []int

	stackBytes   uintptr // stack space used
	iregs, fregs int     // registers used
}

func (a *abiSeq) dump() {
	for i, p := range a.steps {
		println("part", i, p.kind, p.offset, p.size, p.stkOff, p.ireg, p.freg)
	}
	print("values ")
	for _, i := range a.valueStart {
		print(i, " ")
	}
	println()
	println("stack", a.stackBytes)
	println("iregs", a.iregs)
	println("fregs", a.fregs)
}

// stepsForValue returns the ABI instructions for translating
// the i'th Go argument or return value represented by this
// abiSeq to the Go ABI.
func (a *abiSeq) stepsForValue(i int) []abiStep {
	s := a.valueStart[i]
	var e int
	if i == len(a.valueStart)-1 {
		e = len(a.steps)
	} else {
		e = a.valueStart[i+1]
	}
	return a.steps[s:e]
}

// addArg extends the abiSeq with a new Go value of type t.
//
// If the value was stack-assigned, returns the single
// abiStep describing that translation, and nil otherwise.
func (a *abiSeq) addArg(t *rtype) *abiStep {
	pStart := len(a.steps)
	a.valueStart = append(a.valueStart, pStart)
	if !a.regAssign(t, 0) {
		a.steps = a.steps[:pStart]
		a.stackAssign(t.size, uintptr(t.align))
		return &a.steps[len(a.steps)-1]
	}
	return nil
}

// addRcvr extends the abiSeq with a new method call
// receiver according to the interface calling convention.
//
// If the receiver was stack-assigned, returns the single
// abiStep describing that translation, and nil otherwise.
// Returns true if the receiver is a pointer.
func (a *abiSeq) addRcvr(rcvr *rtype) (*abiStep, bool) {
	// The receiver is always one word.
	a.valueStart = append(a.valueStart, len(a.steps))
	var ok, ptr bool
	if ifaceIndir(rcvr) || rcvr.pointers() {
		ok = a.assignIntN(0, ptrSize, 1, 0b1)
		ptr = true
	} else {
		// TODO(mknyszek): Is this case even possible?
		// The interface data work never contains a non-pointer
		// value. This case was copied over from older code
		// in the reflect package which only conditionally added
		// a pointer bit to the reflect.(Value).Call stack frame's
		// GC bitmap.
		ok = a.assignIntN(0, ptrSize, 1, 0b0)
		ptr = false
	}
	if !ok {
		a.stackAssign(ptrSize, ptrSize)
		return &a.steps[len(a.steps)-1], ptr
	}
	return nil, ptr
}

// regAssign attempts to reserve argument registers for a value of
// type t, stored at some offset.
//
// It returns whether or not the assignment succeeded, but
// leaves any changes it made to a.steps behind, so the caller
// must undo that work by adjusting a.steps if it fails.
//
// This method along with the assign* methods represent the
// complete register-assignment algorithm for the Go ABI.
func (a *abiSeq) regAssign(t *rtype, offset uintptr) bool {
	switch t.Kind() {
	case UnsafePointer, Ptr, Chan, Map, Func:
		return a.assignIntN(offset, t.size, 1, 0b1)
	case Bool, Int, Uint, Int8, Uint8, Int16, Uint16, Int32, Uint32, Uintptr:
		return a.assignIntN(offset, t.size, 1, 0b0)
	case Int64, Uint64:
		switch ptrSize {
		case 4:
			return a.assignIntN(offset, 4, 2, 0b0)
		case 8:
			return a.assignIntN(offset, 8, 1, 0b0)
		}
	case Float32, Float64:
		return a.assignFloatN(offset, t.size, 1)
	case Complex64:
		return a.assignFloatN(offset, 4, 2)
	case Complex128:
		return a.assignFloatN(offset, 8, 2)
	case String:
		return a.assignIntN(offset, ptrSize, 2, 0b01)
	case Interface:
		return a.assignIntN(offset, ptrSize, 2, 0b10)
	case Slice:
		return a.assignIntN(offset, ptrSize, 3, 0b001)
	case Array:
		tt := (*arrayType)(unsafe.Pointer(t))
		switch tt.len {
		case 0:
			// There's nothing to assign, so don't modify
			// a.steps but succeed so the caller doesn't
			// try to stack-assign this value.
			return true
		case 1:
			return a.regAssign(tt.elem, offset)
		default:
			return false
		}
	case Struct:
		if t.size == 0 {
			// There's nothing to assign, so don't modify
			// a.steps but succeed so the caller doesn't
			// try to stack-assign this value.
			return true
		}
		st := (*structType)(unsafe.Pointer(t))
		for i := range st.fields {
			f := &st.fields[i]
			if f.typ.Size() == 0 {
				// Ignore zero-sized fields.
				continue
			}
			if !a.regAssign(f.typ, offset+f.offset()) {
				return false
			}
		}
		return true
	default:
		print("t.Kind == ", t.Kind(), "\n")
		panic("unknown type kind")
	}
	panic("unhandled register assignment path")
}

// assignIntN assigns n values to registers, each "size" bytes large,
// from the data at [offset, offset+n*size) in memory. Each value at
// [offset+i*size, offset+(i+1)*size) for i < n is assigned to the
// next n integer registers.
//
// Bit i in ptrMap indicates whether the i'th value is a pointer.
// n must be <= 8.
//
// Returns whether assignment succeeded.
func (a *abiSeq) assignIntN(offset, size uintptr, n int, ptrMap uint8) bool {
	if n > 8 || n < 0 {
		panic("invalid n")
	}
	if ptrMap != 0 && size != ptrSize {
		panic("non-empty pointer map passed for non-pointer-size values")
	}
	if a.iregs+n > abi.IntArgRegs {
		return false
	}
	for i := 0; i < n; i++ {
		kind := abiStepIntReg
		if ptrMap&(uint8(1)<<i) != 0 {
			kind = abiStepPointer
		}
		a.steps = append(a.steps, abiStep{
			kind:   kind,
			offset: offset + uintptr(i)*size,
			size:   size,
			ireg:   a.iregs,
		})
		a.iregs++
	}
	return true
}

// assignFloatN assigns n values to registers, each "size" bytes large,
// from the data at [offset, offset+n*size) in memory. Each value at
// [offset+i*size, offset+(i+1)*size) for i < n is assigned to the
// next n floating-point registers.
//
// Returns whether assignment succeeded.
func (a *abiSeq) assignFloatN(offset, size uintptr, n int) bool {
	if n < 0 {
		panic("invalid n")
	}
	if a.fregs+n > abi.FloatArgRegs || abi.EffectiveFloatRegSize < size {
		return false
	}
	for i := 0; i < n; i++ {
		a.steps = append(a.steps, abiStep{
			kind:   abiStepFloatReg,
			offset: offset + uintptr(i)*size,
			size:   size,
			freg:   a.fregs,
		})
		a.fregs++
	}
	return true
}

// stackAssign reserves space for one value that is "size" bytes
// large with alignment "alignment" to the stack.
//
// Should not be called directly; use addArg instead.
func (a *abiSeq) stackAssign(size, alignment uintptr) {
	a.stackBytes = align(a.stackBytes, alignment)
	a.steps = append(a.steps, abiStep{
		kind:   abiStepStack,
		offset: 0, // Only used for whole arguments, so the memory offset is 0.
		size:   size,
		stkOff: a.stackBytes,
	})
	a.stackBytes += size
}

// abiDesc describes the ABI for a function or method.
type abiDesc struct {
	// call and ret represent the translation steps for
	// the call and return paths of a Go function.
	call, ret abiSeq

	// These fields describe the stack space allocated
	// for the call. stackCallArgsSize is the amount of space
	// reserved for arguments but not return values. retOffset
	// is the offset at which return values begin, and
	// spill is the size in bytes of additional space reserved
	// to spill argument registers into in case of preemption in
	// reflectcall's stack frame.
	stackCallArgsSize, retOffset, spill uintptr

	// stackPtrs is a bitmap that indicates whether
	// each word in the ABI stack space (stack-assigned
	// args + return values) is a pointer. Used
	// as the heap pointer bitmap for stack space
	// passed to reflectcall.
	stackPtrs *bitVector

	// outRegPtrs is a bitmap whose i'th bit indicates
	// whether the i'th integer result register contains
	// a pointer. Used by reflectcall to make result
	// pointers visible to the GC.
	outRegPtrs abi.IntArgRegBitmap
}

func (a *abiDesc) dump() {
	println("ABI")
	println("call")
	a.call.dump()
	println("ret")
	a.ret.dump()
	println("stackCallArgsSize", a.stackCallArgsSize)
	println("retOffset", a.retOffset)
	println("spill", a.spill)
}

func newAbiDesc(t *funcType, rcvr *rtype) abiDesc {
	// We need to add space for this argument to
	// the frame so that it can spill args into it.
	//
	// The size of this space is just the sum of the sizes
	// of each register-allocated type.
	//
	// TODO(mknyszek): Remove this when we no longer have
	// caller reserved spill space.
	spill := uintptr(0)

	// Compute gc program & stack bitmap for stack arguments
	stackPtrs := new(bitVector)

	// Compute abiSeq for input parameters.
	var in abiSeq
	if rcvr != nil {
		stkStep, isPtr := in.addRcvr(rcvr)
		if stkStep != nil {
			if isPtr {
				stackPtrs.append(1)
			} else {
				stackPtrs.append(0)
			}
		} else {
			spill += ptrSize
		}
	}
	for _, arg := range t.in() {
		stkStep := in.addArg(arg)
		if stkStep != nil {
			addTypeBits(stackPtrs, stkStep.stkOff, arg)
		} else {
			spill = align(spill, uintptr(arg.align))
			spill += arg.size
		}
	}
	spill = align(spill, ptrSize)

	// From the input parameters alone, we now know
	// the stackCallArgsSize and retOffset.
	stackCallArgsSize := in.stackBytes
	retOffset := align(in.stackBytes, ptrSize)

	// Compute the stack frame pointer bitmap and register
	// pointer bitmap for return values.
	outRegPtrs := abi.IntArgRegBitmap{}

	// Compute abiSeq for output parameters.
	var out abiSeq
	// Stack-assigned return values do not share
	// space with arguments like they do with registers,
	// so we need to inject a stack offset here.
	// Fake it by artifically extending stackBytes by
	// the return offset.
	out.stackBytes = retOffset
	for i, res := range t.out() {
		stkStep := out.addArg(res)
		if stkStep != nil {
			addTypeBits(stackPtrs, stkStep.stkOff, res)
		} else {
			for _, st := range out.stepsForValue(i) {
				if st.kind == abiStepPointer {
					outRegPtrs.Set(st.ireg)
				}
			}
		}
	}
	// Undo the faking from earlier so that stackBytes
	// is accurate.
	out.stackBytes -= retOffset
	return abiDesc{in, out, stackCallArgsSize, retOffset, spill, stackPtrs, outRegPtrs}
}
