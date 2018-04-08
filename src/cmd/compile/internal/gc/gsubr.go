// Derived from Inferno utils/6c/txt.c
// https://bitbucket.org/inferno-os/inferno-os/src/default/utils/6c/txt.c
//
//	Copyright © 1994-1999 Lucent Technologies Inc.  All rights reserved.
//	Portions Copyright © 1995-1997 C H Forsyth (forsyth@terzarima.net)
//	Portions Copyright © 1997-1999 Vita Nuova Limited
//	Portions Copyright © 2000-2007 Vita Nuova Holdings Limited (www.vitanuova.com)
//	Portions Copyright © 2004,2006 Bruce Ellis
//	Portions Copyright © 2005-2007 C H Forsyth (forsyth@terzarima.net)
//	Revisions Copyright © 2000-2007 Lucent Technologies Inc. and others
//	Portions Copyright © 2009 The Go Authors. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package gc

import (
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/objabi"
	"cmd/internal/src"
)

var sharedProgArray = new([10000]obj.Prog) // *T instead of T to work around issue 19839

// Progs accumulates Progs for a function and converts them into machine code.
type Progs struct {
	Text      *obj.Prog  // ATEXT Prog for this function
	next      *obj.Prog  // next Prog
	pc        int64      // virtual PC; count of Progs
	pos       src.XPos   // position to use for new Progs
	curfn     *Node      // fn these Progs are for
	progcache []obj.Prog // local progcache
	cacheidx  int        // first free element of progcache
}

// newProgs returns a new Progs for fn.
// worker indicates which of the backend workers will use the Progs.
func newProgs(fn *Node, worker int) *Progs {
	pp := new(Progs)
	if Ctxt.CanReuseProgs() {
		sz := len(sharedProgArray) / nBackendWorkers
		pp.progcache = sharedProgArray[sz*worker : sz*(worker+1)]
	}
	pp.curfn = fn

	// prime the pump
	pp.next = pp.NewProg()
	pp.clearp(pp.next)

	pp.pos = fn.Pos
	pp.settext(fn)
	return pp
}

func (pp *Progs) NewProg() *obj.Prog {
	var p *obj.Prog
	if pp.cacheidx < len(pp.progcache) {
		p = &pp.progcache[pp.cacheidx]
		pp.cacheidx++
	} else {
		p = new(obj.Prog)
	}
	p.Ctxt = Ctxt
	return p
}

// Flush converts from pp to machine code.
func (pp *Progs) Flush() {
	plist := &obj.Plist{Firstpc: pp.Text, Curfn: pp.curfn}
	obj.Flushplist(Ctxt, plist, pp.NewProg, myimportpath)
}

// Free clears pp and any associated resources.
func (pp *Progs) Free() {
	if Ctxt.CanReuseProgs() {
		// Clear progs to enable GC and avoid abuse.
		s := pp.progcache[:pp.cacheidx]
		for i := range s {
			s[i] = obj.Prog{}
		}
	}
	// Clear pp to avoid abuse.
	*pp = Progs{}
}

// Prog adds a Prog with instruction As to pp.
func (pp *Progs) Prog(as obj.As) *obj.Prog {
	p := pp.next
	pp.next = pp.NewProg()
	pp.clearp(pp.next)
	p.Link = pp.next

	if !pp.pos.IsKnown() && Debug['K'] != 0 {
		Warn("prog: unknown position (line 0)")
	}

	p.As = as
	p.Pos = pp.pos
	return p
}

func (pp *Progs) clearp(p *obj.Prog) {
	obj.Nopout(p)
	p.As = obj.AEND
	p.Pc = pp.pc
	pp.pc++
}

func (pp *Progs) Appendpp(p *obj.Prog, as obj.As, ftype obj.AddrType, freg int16, foffset int64, ttype obj.AddrType, treg int16, toffset int64) *obj.Prog {
	q := pp.NewProg()
	pp.clearp(q)
	q.As = as
	q.Pos = p.Pos
	q.From.Type = ftype
	q.From.Reg = freg
	q.From.Offset = foffset
	q.To.Type = ttype
	q.To.Reg = treg
	q.To.Offset = toffset
	q.Link = p.Link
	p.Link = q
	return q
}

func (pp *Progs) settext(fn *Node) {
	if pp.Text != nil {
		Fatalf("Progs.settext called twice")
	}
	ptxt := pp.Prog(obj.ATEXT)
	pp.Text = ptxt

	if fn.Func.lsym == nil {
		// func _() { }
		return
	}

	fn.Func.lsym.Func.Text = ptxt
	ptxt.From.Type = obj.TYPE_MEM
	ptxt.From.Name = obj.NAME_EXTERN
	ptxt.From.Sym = fn.Func.lsym

	p := pp.Prog(obj.AFUNCDATA)
	Addrconst(&p.From, objabi.FUNCDATA_ArgsPointerMaps)
	p.To.Type = obj.TYPE_MEM
	p.To.Name = obj.NAME_EXTERN
	p.To.Sym = &fn.Func.lsym.Func.GCArgs

	p = pp.Prog(obj.AFUNCDATA)
	Addrconst(&p.From, objabi.FUNCDATA_LocalsPointerMaps)
	p.To.Type = obj.TYPE_MEM
	p.To.Name = obj.NAME_EXTERN
	p.To.Sym = &fn.Func.lsym.Func.GCLocals
}

func (f *Func) initLSym() {
	if f.lsym != nil {
		Fatalf("Func.initLSym called twice")
	}

	if nam := f.Nname; !nam.isBlank() {
		f.lsym = nam.Sym.Linksym()
		if f.Pragma&Systemstack != 0 {
			f.lsym.Set(obj.AttrCFunc, true)
		}
	}

	var flag int
	if f.Dupok() {
		flag |= obj.DUPOK
	}
	if f.Wrapper() {
		flag |= obj.WRAPPER
	}
	if f.Needctxt() {
		flag |= obj.NEEDCTXT
	}
	if f.Pragma&Nosplit != 0 {
		flag |= obj.NOSPLIT
	}
	if f.ReflectMethod() {
		flag |= obj.REFLECTMETHOD
	}

	// Clumsy but important.
	// See test/recover.go for test cases and src/reflect/value.go
	// for the actual functions being considered.
	if myimportpath == "reflect" {
		switch f.Nname.Sym.Name {
		case "callReflect", "callMethod":
			flag |= obj.WRAPPER
		}
	}

	Ctxt.InitTextSym(f.lsym, flag)
}

func ggloblnod(nam *Node) {
	s := nam.Sym.Linksym()
	s.Gotype = ngotype(nam).Linksym()
	flags := 0
	if nam.Name.Readonly() {
		flags = obj.RODATA
	}
	if nam.Type != nil && !types.Haspointers(nam.Type) {
		flags |= obj.NOPTR
	}
	Ctxt.Globl(s, nam.Type.Width, flags)
}

func ggloblsym(s *obj.LSym, width int32, flags int16) {
	if flags&obj.LOCAL != 0 {
		s.Set(obj.AttrLocal, true)
		flags &^= obj.LOCAL
	}
	Ctxt.Globl(s, int64(width), int(flags))
}

func isfat(t *types.Type) bool {
	if t != nil {
		switch t.Etype {
		case TSTRUCT, TARRAY, TSLICE, TSTRING,
			TINTER: // maybe remove later
			return true
		}
	}

	return false
}

func Addrconst(a *obj.Addr, v int64) {
	a.Sym = nil
	a.Type = obj.TYPE_CONST
	a.Offset = v
}

func Patch(p *obj.Prog, to *obj.Prog) {
	if p.To.Type != obj.TYPE_BRANCH {
		Fatalf("patch: not a branch")
	}
	p.To.Val = to
	p.To.Offset = to.Pc
}
