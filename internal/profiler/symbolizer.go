package profiler

import (
	"debug/elf"
	"debug/gosym"
	"io"
	"sort"
	"sync"

	"github.com/ianlancetaylor/demangle"
)

type elfSym struct {
	addr uint64
	end  uint64
	name string
}

type binarySyms struct {
	isGo  bool
	goTab *gosym.Table
	syms  []elfSym // sorted by addr ascending
}

// Symbolizer resolves ELF virtual addresses for native (Go, Rust, C/C++)
// executables and shared libraries without external tools or CGO.
type Symbolizer struct {
	mu      sync.RWMutex
	entries map[string]*binarySyms
	order   []string
	maxSize int
}

// NewSymbolizer returns a bounded symbol table cache.
func NewSymbolizer(maxFiles int) *Symbolizer {
	if maxFiles <= 0 {
		maxFiles = 512
	}
	return &Symbolizer{
		entries: map[string]*binarySyms{},
		maxSize: maxFiles,
	}
}

// Has reports whether fileID has already been indexed (or marked unindexable).
func (s *Symbolizer) Has(fileID string) bool {
	s.mu.RLock()
	_, ok := s.entries[fileID]
	s.mu.RUnlock()
	return ok
}

// IndexELF parses symbols from an ELF ReaderAt and stores them under fileID.
func (s *Symbolizer) IndexELF(fileID string, r io.ReaderAt) {
	if s.Has(fileID) {
		return
	}
	bs := parseELFSymbols(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[fileID]; exists {
		return
	}
	if len(s.order) >= s.maxSize {
		evict := s.order[0]
		s.order = s.order[1:]
		delete(s.entries, evict)
	}
	s.entries[fileID] = bs
	s.order = append(s.order, fileID)
}

const maxZeroSizeSymSlack = 64 << 10 // 64 KiB max attribution span for ELF symbols with Size == 0

// Lookup resolves addr (ELF virtual address) within fileID.
func (s *Symbolizer) Lookup(fileID string, addr uint64) (name, sourceFile string, sourceLine int32, isGo, ok bool) {
	s.mu.RLock()
	bs := s.entries[fileID]
	s.mu.RUnlock()
	if bs == nil {
		return "", "", 0, false, false
	}
	if bs.goTab != nil {
		if file, line, fn := bs.goTab.PCToLine(addr); fn != nil {
			return fn.Name, file, int32(line), true, true
		}
	}
	if len(bs.syms) == 0 {
		return "", "", 0, bs.isGo, false
	}
	i := sort.Search(len(bs.syms), func(idx int) bool {
		return bs.syms[idx].addr > addr
	}) - 1
	if i < 0 {
		return "", "", 0, bs.isGo, false
	}
	sym := bs.syms[i]
	end := sym.end
	if end <= sym.addr {
		// Symbol had Size == 0 (e.g. assembly label): bound it to the next
		// symbol's address, capped at maxZeroSizeSymSlack so out-of-range
		// addresses are never misattributed to it.
		end = sym.addr + maxZeroSizeSymSlack
		if end < sym.addr {
			end = ^uint64(0)
		}
		if i+1 < len(bs.syms) && bs.syms[i+1].addr < end {
			end = bs.syms[i+1].addr
		}
	}
	if addr >= end {
		return "", "", 0, bs.isGo, false
	}
	return sym.name, "", 0, bs.isGo, true
}

func parseELFSymbols(r io.ReaderAt) *binarySyms {
	bs := &binarySyms{}
	ef, err := elf.NewFile(r)
	if err != nil {
		return bs
	}
	defer ef.Close()

	// Try Go .gopclntab first (works even on stripped Go binaries!).
	if pclndatSec := ef.Section(".gopclntab"); pclndatSec != nil {
		if pclndat, err := pclndatSec.Data(); err == nil && len(pclndat) > 0 {
			var textStart uint64
			if textSec := ef.Section(".text"); textSec != nil {
				textStart = textSec.Addr
			}
			var symdat []byte
			if symSec := ef.Section(".gosymtab"); symSec != nil {
				symdat, _ = symSec.Data()
			}
			lt := gosym.NewLineTable(pclndat, textStart)
			if tab, err := gosym.NewTable(symdat, lt); err == nil {
				bs.goTab = tab
				bs.isGo = true
			}
		}
	}

	// Collect .symtab and .dynsym function symbols (covers C, C++, Rust, libc, etc.).
	var syms []elfSym
	addSyms := func(list []elf.Symbol, err error) {
		if err != nil {
			return
		}
		for _, sym := range list {
			if sym.Value == 0 || sym.Name == "" {
				continue
			}
			typ := elf.ST_TYPE(sym.Info)
			if typ != elf.STT_FUNC && typ != elf.STT_NOTYPE {
				continue
			}
			end := uint64(0)
			if sym.Size > 0 {
				end = sym.Value + sym.Size
			}
			syms = append(syms, elfSym{
				addr: sym.Value,
				end:  end,
				name: demangleSymbol(sym.Name),
			})
		}
	}
	addSyms(ef.Symbols())
	addSyms(ef.DynamicSymbols())

	if len(syms) > 0 {
		sort.Slice(syms, func(i, j int) bool {
			if syms[i].addr != syms[j].addr {
				return syms[i].addr < syms[j].addr
			}
			return syms[i].end > syms[j].end
		})
		// Deduplicate identical start addresses, keeping the first (largest end).
		dedup := syms[:0]
		for _, sym := range syms {
			if len(dedup) > 0 && dedup[len(dedup)-1].addr == sym.addr {
				continue
			}
			dedup = append(dedup, sym)
		}
		bs.syms = dedup
	}
	return bs
}

func demangleSymbol(name string) string {
	// Fast path: only run demangler on C++ Itanium (_Z) or Rust (_R, _ZN) symbols.
	if len(name) >= 2 && name[0] == '_' && (name[1] == 'Z' || name[1] == 'R') {
		if out, err := demangle.ToString(name, demangle.NoParams, demangle.NoTemplateParams); err == nil && out != "" {
			return out
		}
	}
	return name
}
