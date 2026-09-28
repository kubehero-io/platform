// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"debug/elf"
	"debug/gosym"
	"errors"
	"fmt"
	"io"
)

// Limits on what one binary may cost to symbolize. Binaries come from
// arbitrary containers, so parsing is bounded in size and panics are
// contained (see readELFSymbols).
const (
	maxSymtabBytes  = 128 << 20 // .symtab + .strtab read into memory
	maxPclntabBytes = 128 << 20
	maxELFSymbols   = 1 << 21
)

// elfLoad is one PT_LOAD segment: bytes [off, off+filesz) of the file
// are mapped at link-time address vaddr.
type elfLoad struct {
	off, filesz, vaddr uint64
}

// elfSymbols is what user-stack symbolization needs from one binary.
type elfSymbols struct {
	loads  []elfLoad
	table  *symTable // nil when the binary carries no usable symbols
	source string    // "symtab", "gopclntab" or "dynsym"; "" when table is nil
}

// vaddr converts a file offset (from /proc/<pid>/maps: addr - start +
// offset) into the link-time virtual address symbol tables use. This is
// what makes PIE executables and shared libraries, loaded at a random
// base, resolvable with the addresses in their own symbol tables.
func (e *elfSymbols) vaddr(fileOff uint64) (uint64, bool) {
	for _, l := range e.loads {
		if fileOff >= l.off && fileOff-l.off < l.filesz {
			return fileOff - l.off + l.vaddr, true
		}
	}
	return 0, false
}

// lookupFileOffset symbolizes the instruction at a file offset.
func (e *elfSymbols) lookupFileOffset(fileOff uint64) (string, bool) {
	if e == nil || e.table == nil {
		return "", false
	}
	va, ok := e.vaddr(fileOff)
	if !ok {
		return "", false
	}
	return e.table.lookup(va)
}

// readELFSymbols loads PT_LOAD segments and the best available function
// symbol table: .symtab, else .gopclntab for (stripped) Go binaries, else
// .dynsym. Symbols from .symtab/.dynsym are already root-relative link
// addresses; Go's pclntab is rebased onto its text start.
func readELFSymbols(r io.ReaderAt) (es *elfSymbols, err error) {
	defer func() {
		// debug/elf and debug/gosym are hardened but not guaranteed
		// panic-free on hostile input, and a crafted binary in some pod
		// must not take the node agent down.
		if p := recover(); p != nil {
			es, err = nil, fmt.Errorf("elf: parser panic: %v", p)
		}
	}()

	f, err := elf.NewFile(r)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	es = &elfSymbols{}
	for _, p := range f.Progs {
		if p.Type == elf.PT_LOAD && p.Filesz > 0 {
			es.loads = append(es.loads, elfLoad{off: p.Off, filesz: p.Filesz, vaddr: p.Vaddr})
		}
	}
	if len(es.loads) == 0 {
		return nil, errors.New("elf: no PT_LOAD segments")
	}

	if syms, ok := elfFuncSymbols(f, elf.SHT_SYMTAB); ok {
		es.table, es.source = newSymTable(syms), "symtab"
		return es, nil
	}
	if syms, ok := goPclntabSymbols(f); ok {
		es.table, es.source = newSymTable(syms), "gopclntab"
		return es, nil
	}
	if syms, ok := elfFuncSymbols(f, elf.SHT_DYNSYM); ok {
		es.table, es.source = newSymTable(syms), "dynsym"
	}
	return es, nil
}

// elfFuncSymbols returns the defined function symbols of the symbol
// table of the given type, or ok=false when it is missing, empty or over
// budget.
func elfFuncSymbols(f *elf.File, typ elf.SectionType) ([]symbol, bool) {
	sec := f.SectionByType(typ)
	if sec == nil {
		return nil, false
	}
	size := sec.Size
	if int(sec.Link) > 0 && int(sec.Link) < len(f.Sections) {
		size += f.Sections[sec.Link].Size // the string table
	}
	if size > maxSymtabBytes {
		return nil, false
	}
	var all []elf.Symbol
	var err error
	if typ == elf.SHT_SYMTAB {
		all, err = f.Symbols()
	} else {
		all, err = f.DynamicSymbols()
	}
	if err != nil {
		return nil, false
	}
	syms := make([]symbol, 0, len(all)/2)
	for _, s := range all {
		t := elf.ST_TYPE(s.Info)
		if (t != elf.STT_FUNC && t != elf.STT_GNU_IFUNC) || s.Section == elf.SHN_UNDEF || s.Value == 0 || s.Name == "" {
			continue
		}
		if len(syms) >= maxELFSymbols {
			break
		}
		rank := 2
		switch elf.ST_BIND(s.Info) {
		case elf.STB_GLOBAL:
			rank = 0
		case elf.STB_WEAK:
			rank = 1
		}
		syms = append(syms, symbol{addr: s.Value, size: s.Size, name: s.Name, rank: rank})
	}
	return syms, len(syms) > 0
}

// goPclntabSymbols recovers function names from a Go binary's pclntab,
// which survives `-ldflags=-s` stripping because the runtime needs it
// for tracebacks.
func goPclntabSymbols(f *elf.File) ([]symbol, bool) {
	sec := f.Section(".gopclntab")
	if sec == nil {
		sec = f.Section(".data.rel.ro.gopclntab") // PIE
	}
	if sec == nil || sec.Size == 0 || sec.Size > maxPclntabBytes {
		return nil, false
	}
	data, err := sec.Data()
	if err != nil {
		return nil, false
	}
	tab, err := gosym.NewTable(nil, gosym.NewLineTable(data, goTextStart(f, data)))
	if err != nil {
		return nil, false
	}
	syms := make([]symbol, 0, len(tab.Funcs))
	for i := range tab.Funcs {
		fn := &tab.Funcs[i]
		if fn.Entry == 0 || fn.End <= fn.Entry {
			continue
		}
		if len(syms) >= maxELFSymbols {
			break
		}
		syms = append(syms, symbol{addr: fn.Entry, size: fn.End - fn.Entry, name: fn.Name})
	}
	return syms, len(syms) > 0
}

// Magic numbers of the pclntab formats that carry a textStart word
// (Go 1.18 and Go 1.20+).
const (
	go118PclntabMagic = 0xfffffff0
	go120PclntabMagic = 0xfffffff1
)

// goTextStart is the address function offsets in a Go 1.18+ pclntab are
// relative to (runtime.text). The header records it (word 2), which also
// covers externally linked binaries where C code precedes runtime.text.
// It reads as zero in PIE files, where only a dynamic relocation fills it
// in; the .text section start is the right base there. Older formats
// store absolute entry PCs, so the value is unused for them.
func goTextStart(f *elf.File, pclntab []byte) uint64 {
	if len(pclntab) >= 8 {
		magic := f.ByteOrder.Uint32(pclntab)
		ptrSize := int(pclntab[7])
		if (magic == go118PclntabMagic || magic == go120PclntabMagic) &&
			(ptrSize == 4 || ptrSize == 8) && len(pclntab) >= 8+3*ptrSize {
			word := pclntab[8+2*ptrSize : 8+3*ptrSize]
			var v uint64
			if ptrSize == 8 {
				v = f.ByteOrder.Uint64(word)
			} else {
				v = uint64(f.ByteOrder.Uint32(word))
			}
			if v != 0 {
				return v
			}
		}
	}
	if text := f.Section(".text"); text != nil {
		return text.Addr
	}
	return 0
}
