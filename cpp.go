// Copyright 2016 The CC Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cc

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cznic/golex/lex"
	"github.com/cznic/xc"
)

const (
	maxIncludeLevel = 100
)

var (
	protectedMacros = map[int]bool{
		idDate:             true,
		idDefined:          true,
		idFile:             true,
		idLine:             true,
		idSTDC:             true,
		idSTDCHosted:       true,
		idSTDCMBMightNeqWc: true,
		idSTDCVersion:      true,
		idTime:             true,
		idVAARGS:           true,
	}
)

// Macro represents a C preprocessor macro.
type Macro struct {
	Args     []int       // Numeric IDs of argument identifiers.
	DefTok   xc.Token    // Macro name definition token.
	IsFnLike bool        // Whether the macro is function like.
	Value    interface{} // Non nil if macro expands to a constant expression.
	Type     Type        // Non nil if macro expands to a constant expression.
	repl     PPTokenList
}

// ReplacementToks returns the tokens that replace m.
func (m *Macro) ReplacementToks() (r []xc.Token) { return decodeTokens(m.repl, nil, false) }

func (m *Macro) findArg(nm int) int {
	for i, v := range m.Args {
		if v == nm {
			return i
		}
	}

	return -1
}

type macros struct {
	m       map[int]*Macro
	nonRepl []int
	pp      *pp
}

func newMacros() *macros { return &macros{m: map[int]*Macro{}} }

func (m *macros) macros() map[int]*Macro {
	p := m.pp
	defer func(ie bool) {
		p.report.IgnoreErrors = ie
	}(p.report.IgnoreErrors)

	p.report.IgnoreErrors = true
	r := map[int]*Macro{}
	for id, macro := range m.m {
		r[id] = macro

		if macro.IsFnLike {
			continue
		}

		rl := macro.repl
		if rl == 0 {
			macro.Value = true // #define foo -> foo: true.
			macro.Type = p.model.BoolType
			continue
		}

		macro.Value, macro.Type = p.lx.parsePPConstExpr0(rl, p)
	}
	return r
}

type tokenReader interface {
	eof(more bool) bool
	peek() xc.Token
	read() xc.Token
}

type tokenBuf struct {
	toks []xc.Token
}

// Implements tokenReader.
func (t *tokenBuf) eof(bool) bool { return len(t.toks) == 0 }

// Implements tokenReader.
func (t *tokenBuf) peek() xc.Token { return t.toks[0] }

// Implements tokenReader.
func (t *tokenBuf) read() xc.Token { r := t.peek(); t.toks = t.toks[1:]; return r }

type tokenPipe struct {
	ack     chan struct{}
	ackMore bool
	closed  bool
	in      []xc.Token
	out     []xc.Token
	r       chan []xc.Token
	w       chan []xc.Token
}

// Implements tokenReader.
func (t *tokenPipe) eof(more bool) bool {
	if len(t.in) != 0 {
		return false
	}

	if t.closed {
		return true
	}

	t.flush(false)
	if !more {
		return true
	}

	if t.ackMore {
		t.ack <- struct{}{}
	}
	var ok bool
	if t.in, ok = <-t.r; !ok {
		t.closed = true
		return true
	}

	return false
}

// Implements tokenReader.
func (t *tokenPipe) peek() xc.Token { return t.in[0] }

// Implements tokenReader.
func (t *tokenPipe) read() xc.Token { r := t.peek(); t.in = t.in[1:]; return r }

func (t *tokenPipe) flush(final bool) {
	t.out = trimSpace(t.out)
	if n := len(t.out); !final && n != 0 {
		if tok := t.out[n-1]; tok.Rune == STRINGLITERAL || tok.Rune == LONGSTRINGLITERAL {
			// Accumulate lines b/c of possible string concatenation of preprocessing phase 6.
			return
		}
	}

	w := 0
	for _, v := range t.out {
		switch v.Rune {
		case ' ':
			// nop
		default:
			t.out[w] = v
			w++
		}
	}
	t.out = t.out[:w]
	if len(t.out) == 0 {
		return
	}

	// Preproc phase 6. Adjacent string literal tokens are concatenated.
	w = 0
	for r := 0; r < len(t.out); r++ {
		v := t.out[r]
		switch v.Rune {
		case STRINGLITERAL, LONGSTRINGLITERAL:
			to := r
			for to < len(t.out)-1 && t.out[to+1].Rune == STRINGLITERAL {
				to++
			}
			if to == r {
				t.out[w] = v
				w++
				break
			}

			var buf bytes.Buffer
			s := v.S()
			s = s[:len(s)-1] // Remove trailing "
			buf.Write(s)
			for i := r + 1; i <= to; i++ {
				s = dict.S(t.out[i].Val)
				s = s[1 : len(s)-1] // Remove leading and trailing "
				buf.Write(s)
			}
			r = to
			buf.WriteByte('"')
			v.Val = dict.ID(buf.Bytes())
			fallthrough
		default:
			t.out[w] = v
			w++
		}
	}
	t.w <- t.out[:w]
	t.out = nil
}

type pp struct {
	ack             chan struct{}      // Must be unbuffered.
	expandingMacros map[int]int        //
	in              chan []xc.Token    // Must be unbuffered.
	includeLevel    int                //
	includes        []string           //
	lx              *lexer             //
	macros          *macros            //
	model           *Model             //
	ppf             *PreprocessingFile //
	protectMacros   bool               //
	report          *xc.Report         //
	sysIncludes     []string           //
	tweaks          *tweaks            //
}

func newPP(ch chan []xc.Token, includes, sysIncludes []string, macros *macros, protectMacros bool, model *Model, report *xc.Report, tweaks *tweaks) *pp {
	pp := &pp{
		ack:             make(chan struct{}),
		expandingMacros: map[int]int{},
		in:              make(chan []xc.Token),
		includes:        append(includes[:len(includes):len(includes)], sysIncludes...),
		lx:              newSimpleLexer(nil, report, tweaks),
		macros:          macros,
		model:           model,
		protectMacros:   protectMacros,
		report:          report,
		sysIncludes:     sysIncludes,
		tweaks:          tweaks,
	}
	macros.pp = pp
	pp.lx.model = model
	model.initialize(pp.lx)
	go pp.pp2(ch)
	return pp
}

func (p *pp) pp2(ch chan []xc.Token) {
	pipe := &tokenPipe{ack: p.ack, r: p.in, w: ch}
	for !pipe.eof(true) {
		pipe.ackMore = true
		p.expand(pipe, false, func(toks []xc.Token) { pipe.out = append(pipe.out, toks...) })
		pipe.ackMore = false
		p.ack <- struct{}{}
	}
	pipe.flush(true)
	p.ack <- struct{}{}
}

func (p *pp) checkCompatibleReplacementTokenList(tok xc.Token, oldList, newList PPTokenList) {
	ex := trimSpace(decodeTokens(oldList, nil, true))
	toks := trimSpace(decodeTokens(newList, nil, true))

	if g, e := len(toks), len(ex); g != e && len(ex) > 0 {
		p.report.ErrTok(tok, "cannot redefine macro using a replacement list of different length")
		return
	}

	if len(toks) == 0 || len(ex) == 0 {
		return
	}

	if g, e := whitespace(toks), whitespace(ex); !bytes.Equal(g, e) {
		p.report.ErrTok(tok, "cannot redefine macro, whitespace differs")
	}

	for i, g := range toks {
		if e := ex[i]; g.Rune != e.Rune || g.Val != e.Val {
			p.report.ErrTok(tok, "cannot redefine macro using a different replacement list")
			return
		}
	}
}

func (p *pp) defineMacro(tok xc.Token, repl PPTokenList) {
	nm := tok.Val
	if protectedMacros[nm] && p.protectMacros {
		p.report.ErrTok(tok, "cannot define protected macro")
		return
	}

	m := p.macros.m[nm]
	if m == nil {
		p.macros.m[nm] = &Macro{DefTok: tok, repl: repl}
		return
	}

	if m.IsFnLike {
		p.report.ErrTok(tok, "cannot redefine a function-like macro using an object-like macro")
		return
	}

	p.checkCompatibleReplacementTokenList(tok, m.repl, repl)
}

func (p *pp) defineFnMacro(tok xc.Token, il *IdentifierList, repl PPTokenList) {
	nm0 := tok.S()
	nm := dict.ID(nm0[:len(nm0)-1])
	if protectedMacros[nm] && p.protectMacros {
		p.report.ErrTok(tok, "cannot define protected macro")
		return
	}

	var args []int
	for ; il != nil; il = il.IdentifierList {
		tok := il.Token2
		if !tok.IsValid() {
			tok = il.Token
		}
		args = append(args, tok.Val)
	}
	m := p.macros.m[nm]
	defTok := tok
	defTok.Rune = IDENTIFIER
	defTok.Val = nm
	if m == nil {
		p.macros.m[nm] = &Macro{Args: args, DefTok: defTok, IsFnLike: true, repl: repl}
		return
	}

	if !m.IsFnLike {
		p.report.ErrTok(tok, "cannot redefine an object-like macro using a function-like macro")
		return
	}

	if g, e := len(args), len(m.Args); g != e {
		p.report.ErrTok(tok, "cannot redefine macro: number of arguments differ")
		return
	}

	for i, g := range args {
		if e := m.Args[i]; g != e {
			p.report.ErrTok(tok, "cannot redefine macro: argument names differ")
			return
		}
	}

	p.checkCompatibleReplacementTokenList(tok, m.repl, repl)
}

func (p *pp) expand(r tokenReader, handleDefined bool, w func([]xc.Token)) {
	for !r.eof(false) {
		tok := r.read()
		switch tok.Rune {
		case IDENTIFIER:
			if tok.Val == idFile {
				tok.Rune = STRINGLITERAL
				tok.Val = dict.SID(fmt.Sprintf("%q", p.ppf.path))
				w([]xc.Token{tok})
				continue
			}

			if tok.Val == idLine {
				tok.Rune = INTCONST
				tok.Val = dict.SID(strconv.Itoa(position(tok.Pos()).Line))
				w([]xc.Token{tok})
				continue
			}

			if handleDefined && tok.Val == idDefined {
				p.expandDefined(tok, r, w)
				continue
			}

			m := p.macros.m[tok.Val]
			if m == nil {
				w([]xc.Token{tok})
				continue
			}

			p.expandMacro(tok, r, m, handleDefined, w)
		default:
			w([]xc.Token{tok})
		}
	}
}

func (p *pp) expandDefined(tok xc.Token, r tokenReader, w func([]xc.Token)) {
again:
	if r.eof(false) {
		p.report.ErrTok(tok, "'defined' with no argument")
		return
	}

	switch tok = r.read(); tok.Rune {
	case ' ':
		goto again
	case '(': // defined (IDENTIFIER)
		if r.eof(false) {
			p.report.ErrTok(tok, "'defined' with no argument")
			return
		}

		tok = r.read()
		switch tok.Rune {
		case IDENTIFIER:
			v := tok
			v.Rune = INTCONST
			if p.macros.m[tok.Val] != nil {
				v.Val = id1
			} else {
				v.Val = id0
			}

			if r.eof(false) {
				p.report.ErrTok(tok, "must be followed by ')'")
				return
			}

			tok = r.read()
			if tok.Rune != ')' {
				p.report.ErrTok(tok, "expected ')'")
				return
			}

			w([]xc.Token{v})
		default:
			p.report.ErrTok(tok, "expected identifier")
			return
		}
	case IDENTIFIER:
		v := tok
		v.Rune = INTCONST
		if p.macros.m[tok.Val] != nil {
			v.Val = id1
		} else {
			v.Val = id0
		}

		w([]xc.Token{v})
	default:
		panic(PrettyString(tok))
	}
}

func (p *pp) expandMacro(tok xc.Token, r tokenReader, m *Macro, handleDefined bool, w func([]xc.Token)) {
	nm := tok.Val
	p.expandingMacros[nm]++
	defer func() { p.expandingMacros[nm]-- }()

	for _, v := range p.macros.nonRepl {
		if v == nm {
			w([]xc.Token{tok})
			return
		}
	}

	if p.expandingMacros[nm] > 1 {
		p.macros.nonRepl = append(p.macros.nonRepl, nm)
		w([]xc.Token{tok})
		return
	}

	if m.IsFnLike {
		p.expandFnMacro(tok, r, m, handleDefined, w)
		return
	}

	repl := trimSpace(decodeTokens(m.repl, nil, true))
	pos := tok.Pos()
	for i, v := range repl {
		repl[i].Char = lex.NewChar(pos, v.Rune)
	}
	p.expand(
		&tokenBuf{p.expandLineNo(repl, tok)},
		handleDefined,
		w,
	)
}

func (p *pp) expandLineNo(toks []xc.Token, lineTok xc.Token) []xc.Token {
	for i, v := range toks {
		if v.Rune == IDENTIFIER && v.Val == idLine {
			v.Rune = INTCONST
			v.Val = dict.SID(strconv.Itoa(position(lineTok.Pos()).Line))
			toks[i] = v
		}
	}
	return toks
}

func (p *pp) expandFnMacro(tok xc.Token, r tokenReader, m *Macro, handleDefined bool, w func([]xc.Token)) {
again:
	if r.eof(true) {
		w([]xc.Token{tok})
		return
	}

	switch c := r.peek().Rune; {
	case c == ' ':
		r.read()
		goto again
	case c != '(': // != name()
		w([]xc.Token{tok})
		return
	}

	args := p.parseMacroArgs(r)
	if g, e := len(args), len(m.Args); g != e {
		switch {
		case g == 1 && e == 0 && len(args[0]) == 0:
			// Spacial case: Handling of empty args to macros with
			// one parameter makes it non distinguishable of
			// passing no argument to a macro with no parameters.

			// ok, nop.
		default:
			p.report.ErrTok(tok, "macro argument count mismatch: got %v, expected %v", g, e)
			return
		}
	}

	for i, arg := range args {
		args[i] = nil
		p.expand(&tokenBuf{p.expandLineNo(arg, tok)}, handleDefined, func(toks []xc.Token) { args[i] = append(args[i], toks...) })
	}
	for i, arg := range args {
		args[i] = trimSpace(arg)
	}
	repl := trimSpace(decodeTokens(m.repl, nil, true))
	if len(repl) != 0 {
		for i := 0; i < len(repl)-1; {
			switch tok := repl[i]; tok.Rune {
			case ' ':
				if repl[i+1].Rune == PPPASTE {
					repl = append(repl[:i], repl[i+1:]...)
					if i > 0 {
						i--
					}
					break
				}

				if i > 0 && repl[i-1].Rune == PPPASTE {
					repl = append(repl[:i], repl[i+1:]...)
					break
				}

				i++
			default:
				i++
			}
		}
		for i, tok := range repl[:len(repl)-1] {
			switch tok.Rune {
			case PPPASTE:
				if i == 0 || i == len(repl)-1 {
					break
				}

				if tok := repl[i-1]; tok.Rune == IDENTIFIER {
					if ia := m.findArg(tok.Val); ia >= 0 && len(args[ia]) > 1 {
						p.report.ErrTok(args[ia][0], "invalid multitoken argument of ##")
						return
					}
				}

				if tok := repl[i+1]; tok.Rune == IDENTIFIER {
					if ia := m.findArg(tok.Val); ia >= 0 && len(args[ia]) > 1 {
						p.report.ErrTok(args[ia][0], "invalid multitoken argument of ##")
						return
					}
				}
			}
		}
	}
	var r0 []xc.Token
next:
	for i, tok := range repl {
		switch tok.Rune {
		case IDENTIFIER:
			for ia, v := range m.Args {
				if v == tok.Val {
					if i > 0 && repl[i-1].Rune == '#' {
						r0 = append(r0[:len(r0)-1], stringify(args[ia]))
					} else {
						arg := args[ia]
						if len(arg) == 0 {
							arg = []xc.Token{{}}
						}
						r0 = append(r0, arg...)
					}
					continue next
				}
			}

			r0 = append(r0, tok)
		default:
			r0 = append(r0, tok)
		}
	}

	var y []xc.Token
	for i := 0; i < len(r0); {
		tok := r0[i]
		switch {
		case i+1 <= len(r0)-2 && r0[i+1].Rune == PPPASTE:
			tok.Val = dict.ID(append(tok.S(), r0[i+2].S()...))
			i += 2
			r0[i] = tok
		default:
			y = append(y, tok)
			i++
		}
	}

	p.expand(&tokenBuf{p.expandLineNo(y, tok)}, handleDefined, w)
}

func stringify(toks []xc.Token) xc.Token {
	toks = trimSpace(toks)
	if len(toks) == 0 || (toks[0] == xc.Token{}) {
		return xc.Token{Char: lex.NewChar(0, STRINGLITERAL), Val: idEmptyString}
	}

	v := tokVal(toks[0])
	s := append([]byte{'"'}, unquote(dict.S(v))...)
	for _, t := range toks[1:] {
		v = tokVal(t)
		s = append(s, unquote(dict.S(v))...)
	}
	s = append(s, '"')
	return xc.Token{Char: lex.NewChar(toks[0].Pos(), STRINGLITERAL), Val: dict.ID(s)}
}

func unquote(b []byte) []byte {
	if len(b) != 0 && b[0] == '"' {
		b = b[1:]
	}
	if n := len(b); n != 0 && b[n-1] == '"' {
		b = b[:n-1]
	}
	return b
}

func whitespace(toks []xc.Token) []byte {
	if len(toks) < 2 {
		return nil
	}

	r := make([]byte, 0, len(toks)-1)
	ltok := toks[0]
	for _, tok := range toks[1:] {
		if ltok.Rune == ' ' {
			continue
		}

		switch {
		case tok.Rune == ' ':
			r = append(r, 1)
		default:
			r = append(r, 0)
		}
		ltok = tok
	}
	return r
}

func (p *pp) parseMacroArgs(r tokenReader) (args [][]xc.Token) {
	if r.eof(true) {
		panic("internal error")
	}

	tok := r.read()
	if tok.Rune != '(' {
		p.report.ErrTok(tok, "expected '('")
		return nil
	}

	for !r.eof(true) {
		arg, more := p.parseMacroArg(r)
		args = append(args, arg)
		if more {
			continue
		}

		if r.peek().Rune == ')' {
			break
		}
	}

	if r.eof(true) {
		p.report.ErrTok(tok, "missing final ')'")
		return nil
	}

	tok = r.read()
	if tok.Rune != ')' {
		p.report.ErrTok(tok, "expected ')'")
	}

	return args
}

func (p *pp) parseMacroArg(r tokenReader) (arg []xc.Token, more bool) {
	n := 0
	tok := r.peek()
	for {
		if r.eof(true) {
			p.report.ErrTok(tok, "unexpected end of line after token")
			return arg, false
		}

		tok = r.peek()
		switch tok.Rune {
		case '(':
			arg = append(arg, r.read())
			n++
		case ')':
			if n == 0 {
				return arg, false
			}

			arg = append(arg, r.read())
			n--
		case ',':
			if n == 0 {
				r.read()
				return arg, true
			}

			arg = append(arg, r.read())
		default:
			arg = append(arg, r.read())
		}
	}
}

func (p *pp) preprocessingFile(n *PreprocessingFile) {
	ppf := p.ppf
	p.ppf = n
	p.groupList(n.GroupList)
	p.ppf = ppf
	if p.includeLevel == 0 {
		close(p.in)
		<-p.ack
	}
}

func (p *pp) groupList(n *GroupList) {
	for ; n != nil; n = n.GroupList {
		switch gp := n.GroupPart.(type) {
		case nil: // PPNONDIRECTIVE PPTokenList
			// nop
		case *ControlLine:
			p.controlLine(gp)
		case *IfSection:
			p.ifSection(gp)
		case PPTokenList: // TextLine
			if gp == 0 {
				break
			}

			toks := decodeTokens(gp, nil, true)
			p.in <- toks
			<-p.ack
		case xc.Token:
			if p.tweaks.enableWarnings {
				fmt.Printf("[INFO] %s at %s\n", gp.S(), xc.FileSet.Position(gp.Pos()).String())
			}
		default:
			panic("internal error")
		}
	}
}

func (p *pp) ifSection(n *IfSection) {
	_ = p.ifGroup(n.IfGroup) ||
		p.elifGroupListOpt(n.ElifGroupListOpt) ||
		p.elseGroupOpt(n.ElseGroupOpt)
}

func (p *pp) ifGroup(n *IfGroup) bool {
	switch n.Case {
	case 0: // PPIF PPTokenList GroupListOpt
		if !p.lx.parsePPConstExpr(n.PPTokenList, p) {
			return false
		}
	case 1: // PPIFDEF IDENTIFIER '\n' GroupListOpt
		if m := p.macros.m[n.Token2.Val]; m == nil {
			return false
		}
	case 2: // PPIFNDEF IDENTIFIER '\n' GroupListOpt
		if m := p.macros.m[n.Token2.Val]; m != nil {
			return false
		}
	default:
		panic(n.Case)
	}
	p.groupListOpt(n.GroupListOpt)
	return true
}

func (p *pp) elifGroupListOpt(n *ElifGroupListOpt) bool {
	if n == nil {
		return false
	}

	return p.elifGroupList(n.ElifGroupList)
}

func (p *pp) elifGroupList(n *ElifGroupList) bool {
	for ; n != nil; n = n.ElifGroupList {
		if p.elifGroup(n.ElifGroup) {
			return true
		}
	}

	return false
}

func (p *pp) elifGroup(n *ElifGroup) bool {
	if !p.lx.parsePPConstExpr(n.PPTokenList, p) {
		return false
	}

	p.groupListOpt(n.GroupListOpt)
	return true
}

func (p *pp) elseGroupOpt(n *ElseGroupOpt) bool {
	if n == nil {
		return false
	}

	return p.elseGroup(n.ElseGroup)
}

func (p *pp) elseGroup(n *ElseGroup) bool {
	p.groupListOpt(n.GroupListOpt)
	return true
}

func (p *pp) groupListOpt(n *GroupListOpt) {
	if n == nil {
		return
	}

	p.groupList(n.GroupList)
}

func (p *pp) controlLine(n *ControlLine) {
	switch n.Case {
	case 0: // PPDEFINE IDENTIFIER ReplacementList
		p.defineMacro(n.Token2, n.ReplacementList)
	case 1: // PPDEFINE IDENTIFIER_LPAREN "..." ')' ReplacementList
		p.defineFnMacro(n.Token2, nil, n.ReplacementList)
	case 2: // PPDEFINE IDENTIFIER_LPAREN IdentifierList ',' "..." ')' ReplacementList
		p.defineFnMacro(n.Token2, n.IdentifierList, n.ReplacementList)
	case 3: // PPDEFINE IDENTIFIER_LPAREN IdentifierListOpt ')' ReplacementList
		var l *IdentifierList
		if o := n.IdentifierListOpt; o != nil {
			l = o.IdentifierList
		}
		p.defineFnMacro(n.Token2, l, n.ReplacementList)
	case 4: // PPERROR PPTokenListOpt
		var sep string
		toks := decodeTokens(n.PPTokenListOpt, nil, true)
		s := stringify(toks)
		if s.Val != 0 {
			sep = ": "
		}
		p.report.ErrTok(n.Token, "error%s%s", sep, s.S())
	case 6: // PPINCLUDE PPTokenList
		toks := decodeTokens(n.PPTokenList, nil, false)
		if len(toks) == 0 {
			p.report.ErrTok(n.Token, "invalid #include argument")
			break
		}

		if p.includeLevel == maxIncludeLevel {
			p.report.ErrTok(toks[0], "too many include nesting levels")
			break
		}

		arg := string(toks[0].S())
		var dirs []string
		switch {
		case strings.HasPrefix(arg, "<"):
			dirs = p.sysIncludes
		default:
			dirs = p.includes
		}
		// Include origin.
		dirs = append(dirs, filepath.Dir(p.ppf.path))
		arg = arg[1 : len(arg)-1]
		for _, dir := range dirs {
			pth := filepath.Join(dir, arg)
			if _, err := os.Stat(pth); err != nil {
				if !os.IsNotExist(err) {
					p.report.ErrTok(toks[0], err.Error())
				}
				continue
			}

			ppf, err := ppParse(pth, p.report, p.tweaks)
			if err != nil {
				p.report.ErrTok(toks[0], err.Error())
				return
			}

			p.includeLevel++
			p.preprocessingFile(ppf)
			p.includeLevel--
			return
		}

		p.report.ErrTok(toks[0], "include file not found: %s", arg)
	case 8: // PPPRAGMA PPTokenListOpt
		// simply ignore pragmas (#pragma once already works)
		return
	case
		9,  // PPUNDEF IDENTIFIER '\n'
		13: // PPUNDEF IDENTIFIER PPTokenList '\n'
		nm := n.Token2.Val
		if protectedMacros[nm] && p.protectMacros {
			p.report.ErrTok(n.Token2, "cannot undefine protected macro")
			return
		}

		delete(p.macros.m, nm)
	case 14: // PPINCLUDE_NEXT PPTokenList '\n'
		toks := decodeTokens(n.PPTokenList, nil, false)
		if len(toks) == 0 {
			p.report.ErrTok(n.Token, "invalid #include_next argument")
			break
		}

		if p.includeLevel == maxIncludeLevel {
			p.report.ErrTok(toks[0], "too many include nesting levels")
			break
		}

		arg := string(toks[0].S())
		arg = arg[1 : len(arg)-1]
		origin := filepath.Dir(n.Token.Position().Filename)
		var dirs []string
		found := false
		for i, dir := range p.includes {
			if dir == origin {
				dirs = p.includes[i+1:]
				found = true
				break
			}
		}
		if !found {
			for i, dir := range p.sysIncludes {
				if dir == origin {
					dirs = p.sysIncludes[i+1:]
					found = true
					break
				}
			}
		}

		for _, dir := range dirs {
			pth := filepath.Join(dir, arg)
			if _, err := os.Stat(pth); err != nil {
				if !os.IsNotExist(err) {
					p.report.ErrTok(toks[0], err.Error())
				}
				continue
			}

			ppf, err := ppParse(pth, p.report, p.tweaks)
			if err != nil {
				p.report.ErrTok(toks[0], err.Error())
				return
			}

			p.includeLevel++
			p.preprocessingFile(ppf)
			p.includeLevel--
			return
		}

		p.report.ErrTok(toks[0], "include file not found: %s", arg)
	default:
		panic(n.Case)
	}
}
