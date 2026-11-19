package index

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/kistdb/narcissus/internal/sqltext"
)

// Key is one key term of an index: a column, possibly inside text functions,
// with an optional collation.
type Key struct {
	Column  string
	Funcs   []string // outermost first, as in lower(trim(x))
	Collate string
}

// Literal kinds a partial index may compare with.
const (
	Integer = "integer"
	Text    = "text"
	Null    = "null"
)

// Cond is one comparison in a partial index's WHERE clause.
type Cond struct {
	Column, Literal string // Literal is a kind, empty for IS NULL tests
}

// textFuncs may wrap a key column when collations and functions are allowed.
var textFuncs = []string{"lower", "upper", "trim", "ltrim", "rtrim"}

// Shape reads the index as the only kinds Narcissus builds: keys that copy a
// column (perhaps through a text function), and a WHERE of simple comparisons.
func (ix Index) Shape() ([]Key, []Cond, error) {
	toks, err := sqltext.Lex(ix.SQL)
	if err != nil {
		return nil, nil, err
	}
	open := slices.IndexFunc(toks, func(t sqltext.Token) bool { return t.Text == "(" })
	if open < 0 {
		return nil, nil, errors.New("no column list")
	}
	terms, end := sqltext.SplitList(toks[open+1:])
	var keys []Key
	for _, term := range terms {
		k, err := key(term)
		if err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
	}
	rest := toks[min(open+1+end+1, len(toks)):]
	if len(rest) == 0 {
		return keys, nil, nil
	}
	if !rest[0].Is("where") {
		return nil, nil, fmt.Errorf("%q after its columns", rest[0].Text)
	}
	p := &whereParser{toks: rest[1:]}
	if err := p.or(); err != nil {
		return nil, nil, err
	}
	if p.i < len(p.toks) {
		return nil, nil, fmt.Errorf("%q in its WHERE", p.toks[p.i].Text)
	}
	return keys, p.conds, nil
}

func key(term []sqltext.Token) (Key, error) {
	var k Key
	n := len(term)
	if n > 0 && (term[n-1].Is("asc") || term[n-1].Is("desc")) {
		n--
	}
	if n >= 2 && term[n-2].Is("collate") && isName(term[n-1]) {
		k.Collate = term[n-1].Ident()
		n -= 2
	}
	expr := term[:n]
	for len(expr) >= 3 && expr[0].Kind == sqltext.Word && expr[1].Text == "(" && expr[len(expr)-1].Text == ")" {
		if !slices.Contains(textFuncs, sqltext.Fold(expr[0].Text)) {
			return Key{}, fmt.Errorf("%s() in its columns", expr[0].Text)
		}
		k.Funcs = append(k.Funcs, sqltext.Fold(expr[0].Text))
		expr = expr[2 : len(expr)-1]
	}
	if len(expr) != 1 || !isName(expr[0]) {
		return Key{}, errors.New("an expression in its columns")
	}
	k.Column = expr[0].Ident()
	return k, nil
}

func isName(t sqltext.Token) bool {
	return t.Kind == sqltext.Quoted || t.Kind == sqltext.Word && !sqltext.IsKeyword(t.Text)
}

// whereParser reads: or := and {OR and}; and := not {AND not}; not := NOT not |
// ( or ) | predicate.
type whereParser struct {
	toks  []sqltext.Token
	i     int
	conds []Cond
}

func (p *whereParser) peek() sqltext.Token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return sqltext.Token{}
}

func (p *whereParser) accept(word string) bool {
	if p.peek().Is(word) || p.peek().Text == word && word != "" {
		p.i++
		return true
	}
	return false
}

func (p *whereParser) or() error {
	for {
		if err := p.and(); err != nil {
			return err
		}
		if !p.accept("or") {
			return nil
		}
	}
}

func (p *whereParser) and() error {
	for {
		if err := p.not(); err != nil {
			return err
		}
		if !p.accept("and") {
			return nil
		}
	}
}

func (p *whereParser) not() error {
	switch {
	case p.accept("not"):
		return p.not()
	case p.accept("("):
		if err := p.or(); err != nil {
			return err
		}
		if !p.accept(")") {
			return errors.New("an unclosed parenthesis in its WHERE")
		}
		return nil
	}
	return p.predicate()
}

var comparisons = []string{"=", "==", "!=", "<>", "<", "<=", ">", ">="}

// predicate reads a comparison between one column and literals.
func (p *whereParser) predicate() error {
	if lit, ok := p.literal(); ok { // literal op column
		if !slices.Contains(comparisons, p.peek().Text) {
			return errors.New("a comparison without a column in its WHERE")
		}
		p.i++
		col := p.peek()
		if !isName(col) {
			return errors.New("a comparison without a column in its WHERE")
		}
		p.i++
		p.conds = append(p.conds, Cond{col.Ident(), lit})
		return nil
	}
	col := p.peek()
	if !isName(col) {
		return fmt.Errorf("%q in its WHERE", col.Text)
	}
	p.i++
	name := col.Ident()
	switch {
	case slices.Contains(comparisons, p.peek().Text):
		p.i++
		return p.compare(name)
	case p.accept("isnull"), p.accept("notnull"):
		p.conds = append(p.conds, Cond{Column: name})
		return nil
	case p.accept("is"):
		p.accept("not")
		return p.compare(name)
	}
	p.accept("not")
	switch {
	case p.accept("null"):
		p.conds = append(p.conds, Cond{Column: name})
		return nil
	case p.accept("in"):
		if !p.accept("(") {
			return errors.New("an IN without a list of values in its WHERE")
		}
		for {
			if err := p.compare(name); err != nil {
				return err
			}
			if p.accept(")") {
				return nil
			}
			if !p.accept(",") {
				return errors.New("an IN without a list of values in its WHERE")
			}
		}
	case p.accept("between"):
		if err := p.compare(name); err != nil {
			return err
		}
		if !p.accept("and") {
			return errors.New("a BETWEEN without AND in its WHERE")
		}
		return p.compare(name)
	}
	return fmt.Errorf("%q after %s in its WHERE", p.peek().Text, name)
}

// compare reads the literal a column is compared with.
func (p *whereParser) compare(col string) error {
	lit, ok := p.literal()
	if !ok {
		return fmt.Errorf("%s compared with something other than a whole number, text or NULL", col)
	}
	p.conds = append(p.conds, Cond{col, lit})
	return nil
}

// smallInt matches whole numbers every SQLite reads and compares exactly.
var smallInt = regexp.MustCompile(`^[0-9]{1,15}$`)

// literal reads NULL, text, or a whole number with an optional minus. TRUE and
// FALSE aren't literals: old SQLite lacks them, and a column can take the name.
func (p *whereParser) literal() (string, bool) {
	t := p.peek()
	switch {
	case t.Is("null"):
		p.i++
		return Null, true
	case t.Kind == sqltext.String:
		p.i++
		return Text, true
	case t.Kind == sqltext.Number && smallInt.MatchString(t.Text):
		p.i++
		return Integer, true
	case t.Text == "-" && p.i+1 < len(p.toks) && p.toks[p.i+1].Kind == sqltext.Number && smallInt.MatchString(p.toks[p.i+1].Text):
		p.i += 2
		return Integer, true
	}
	return "", false
}

// Describe names what a key computes, for messages.
func (k Key) Describe() string {
	if len(k.Funcs) == 0 {
		return k.Column
	}
	return strings.Join(k.Funcs, "(") + "(" + k.Column + strings.Repeat(")", len(k.Funcs))
}
