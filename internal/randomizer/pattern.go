package randomizer

import (
	"math/rand"
	"strconv"
	"strings"
)

// GeneratePattern produces a string matching a regular expression.
//
// Specs use `pattern` on the fields where a wrong value is rejected outright:
// account numbers, SKUs, postcodes. A random string there guarantees a 400. So
// this is a generation-only regex engine, much simpler than a matcher because
// it only has to produce one member of the language, not decide membership.
//
// It covers what specs actually use: literals, character classes, escapes,
// alternation, non-capturing groups, quantifiers. Lookaround and backreferences
// return ok=false so the caller can fall back to ordinary string generation.
//
// maxRepeat bounds unbounded quantifiers so `.*` cannot produce a megabyte.
const maxRepeat = 8

func GeneratePattern(rng *rand.Rand, pattern string) (string, bool) {
	if pattern == "" {
		return "", false
	}
	// Anchors carry no generative meaning; the output is the whole string.
	trimmed := strings.TrimPrefix(pattern, "^")
	trimmed = strings.TrimSuffix(trimmed, "$")

	p := &patParser{src: []rune(trimmed), rng: rng, ok: true}
	out := p.alternation()
	if !p.ok || p.pos != len(p.src) {
		return "", false
	}
	return out, true
}

type patParser struct {
	src []rune
	pos int
	rng *rand.Rand
	ok  bool
}

func (p *patParser) peek() rune {
	if p.pos >= len(p.src) {
		return 0
	}
	return p.src[p.pos]
}

func (p *patParser) next() rune {
	r := p.peek()
	p.pos++
	return r
}

func (p *patParser) fail() string {
	p.ok = false
	return ""
}

// alternation parses `a|b|c` and returns one branch. All branches are parsed so
// the input is consumed; only the chosen one contributes output.
func (p *patParser) alternation() string {
	branches := []string{p.sequence()}
	for p.ok && p.peek() == '|' {
		p.next()
		branches = append(branches, p.sequence())
	}
	if !p.ok {
		return ""
	}
	return branches[p.rng.Intn(len(branches))]
}

func (p *patParser) sequence() string {
	var sb strings.Builder
	for p.ok && p.pos < len(p.src) && p.peek() != '|' && p.peek() != ')' {
		sb.WriteString(p.quantified())
	}
	return sb.String()
}

// quantified parses one atom plus any trailing quantifier. The atom is a
// generator rather than a string so `[a-z]{4}` yields four different letters
// instead of the same one four times.
func (p *patParser) quantified() string {
	gen := p.atom()
	if !p.ok {
		return ""
	}

	lo, hi, has := p.quantifier()
	if !p.ok {
		return ""
	}
	if !has {
		return gen()
	}

	n := lo
	if hi > lo {
		n = lo + p.rng.Intn(hi-lo+1)
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(gen())
	}
	return sb.String()
}

func (p *patParser) quantifier() (lo, hi int, has bool) {
	switch p.peek() {
	case '*':
		p.next()
		p.maybeLazy()
		return 0, maxRepeat, true
	case '+':
		p.next()
		p.maybeLazy()
		return 1, maxRepeat, true
	case '?':
		p.next()
		p.maybeLazy()
		return 0, 1, true
	case '{':
		return p.braceQuantifier()
	}
	return 0, 0, false
}

// maybeLazy swallows a lazy marker; greediness does not affect generation.
func (p *patParser) maybeLazy() {
	if p.peek() == '?' {
		p.next()
	}
}

func (p *patParser) braceQuantifier() (int, int, bool) {
	start := p.pos
	p.next() // consume '{'

	var body strings.Builder
	for p.pos < len(p.src) && p.peek() != '}' {
		body.WriteRune(p.next())
	}
	if p.peek() != '}' {
		// A literal brace, not a quantifier. Rewind and let atom handle it.
		p.pos = start
		return 0, 0, false
	}
	p.next() // consume '}'

	text := body.String()
	minStr, maxStr, hasComma := strings.Cut(text, ",")

	lo, err := strconv.Atoi(strings.TrimSpace(minStr))
	if err != nil || lo < 0 {
		p.ok = false
		return 0, 0, false
	}

	switch {
	case !hasComma:
		return clampRepeat(lo), clampRepeat(lo), true
	case strings.TrimSpace(maxStr) == "":
		return clampRepeat(lo), clampRepeat(lo + 2), true
	default:
		hi, err := strconv.Atoi(strings.TrimSpace(maxStr))
		if err != nil || hi < lo {
			p.ok = false
			return 0, 0, false
		}
		return clampRepeat(lo), clampRepeat(hi), true
	}
}

// clampRepeat keeps generated strings sane. A spec saying {1,4096} is stating a
// field bound, not asking for 4096 characters.
func clampRepeat(n int) int {
	const hardCap = 64
	if n > hardCap {
		return hardCap
	}
	return n
}

// atom returns a generator for a single regex element.
func (p *patParser) atom() func() string {
	switch c := p.peek(); c {
	case '(':
		return p.group()
	case '[':
		return p.charClass()
	case '\\':
		p.next()
		return p.escape()
	case '.':
		p.next()
		return p.fromSet(alphanumeric)
	case '*', '+', '?':
		// A quantifier with nothing to quantify.
		p.fail()
		return func() string { return "" }
	default:
		p.next()
		s := string(c)
		return func() string { return s }
	}
}

func (p *patParser) group() func() string {
	p.next() // consume '('

	// Non-capturing groups are fine. Other (?...) constructs are lookaround or
	// flags, which have no generative meaning.
	if p.peek() == '?' {
		if p.pos+1 < len(p.src) && p.src[p.pos+1] == ':' {
			p.next()
			p.next()
		} else {
			p.fail()
			return func() string { return "" }
		}
	}

	start := p.pos
	depth := 1
	for p.pos < len(p.src) && depth > 0 {
		switch p.src[p.pos] {
		case '\\':
			p.pos++ // skip whatever is escaped
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				sub := string(p.src[start:p.pos])
				p.pos++ // consume ')'
				rng := p.rng
				return func() string {
					// Regenerate per repetition so (ab|cd){3} can vary.
					out, ok := GeneratePattern(rng, sub)
					if !ok {
						return ""
					}
					return out
				}
			}
		}
		p.pos++
	}

	p.fail() // unbalanced parenthesis
	return func() string { return "" }
}

func (p *patParser) charClass() func() string {
	p.next() // consume '['

	negated := false
	if p.peek() == '^' {
		p.next()
		negated = true
	}

	var set []rune
	for p.pos < len(p.src) && p.peek() != ']' {
		c := p.next()

		if c == '\\' {
			set = append(set, p.escapeSet()...)
			continue
		}
		// A range like a-z, but a trailing '-' before ']' is a literal.
		if p.peek() == '-' && p.pos+1 < len(p.src) && p.src[p.pos+1] != ']' {
			p.next() // consume '-'
			hi := p.next()
			if hi < c {
				p.fail()
				return func() string { return "" }
			}
			for r := c; r <= hi; r++ {
				set = append(set, r)
			}
			continue
		}
		set = append(set, c)
	}

	if p.peek() != ']' {
		p.fail()
		return func() string { return "" }
	}
	p.next() // consume ']'

	if negated {
		set = complement(set)
	}
	if len(set) == 0 {
		p.fail()
		return func() string { return "" }
	}
	return p.fromSet(set)
}

// escape handles a backslash outside a character class.
func (p *patParser) escape() func() string {
	c := p.next()
	if set, ok := escapeSets[c]; ok {
		return p.fromSet(set)
	}
	// A backreference has no generative meaning.
	if c >= '1' && c <= '9' {
		p.fail()
		return func() string { return "" }
	}
	s := string(c)
	return func() string { return s }
}

// escapeSet handles a backslash inside a character class, contributing to the
// enclosing set instead of generating directly.
func (p *patParser) escapeSet() []rune {
	c := p.next()
	if set, ok := escapeSets[c]; ok {
		return set
	}
	return []rune{c}
}

func (p *patParser) fromSet(set []rune) func() string {
	rng := p.rng
	return func() string { return string(set[rng.Intn(len(set))]) }
}

func runeRange(lo, hi rune) []rune {
	out := make([]rune, 0, hi-lo+1)
	for r := lo; r <= hi; r++ {
		out = append(out, r)
	}
	return out
}

var (
	digits       = runeRange('0', '9')
	lowers       = runeRange('a', 'z')
	uppers       = runeRange('A', 'Z')
	letters      = append(append([]rune{}, lowers...), uppers...)
	alphanumeric = append(append([]rune{}, letters...), digits...)
	wordChars    = append(append([]rune{}, alphanumeric...), '_')
)

var escapeSets = map[rune][]rune{
	'd': digits,
	'w': wordChars,
	's': {' '},
	'D': letters,
	'W': {' ', '-', '.'},
	'S': alphanumeric,
	'n': {'\n'},
	't': {'\t'},
}

// complement approximates a negated class over printable ASCII. The full
// Unicode complement would be correct and useless.
func complement(excluded []rune) []rune {
	blocked := make(map[rune]bool, len(excluded))
	for _, r := range excluded {
		blocked[r] = true
	}
	out := make([]rune, 0, len(wordChars))
	for _, r := range wordChars {
		if !blocked[r] {
			out = append(out, r)
		}
	}
	return out
}
