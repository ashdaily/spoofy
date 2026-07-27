package randomizer

import (
	"math/rand"
	"strconv"
	"strings"
)

// GeneratePattern produces a string matching a regular expression.
//
// Specs use `pattern` for exactly the fields where a wrong value is rejected:
// account numbers, SKUs, postcodes, microchip IDs. Generating a random string
// for those guarantees a 400, which is traffic that proves nothing. So this is
// a small generation-only regex engine — far simpler than a matcher, since it
// only has to produce one member of the language rather than decide membership.
//
// It handles the constructs specs actually use: literals, character classes,
// escapes, alternation, non-capturing groups, and quantifiers. Anything beyond
// that (lookaround, backreferences) returns ok=false, and the caller falls back
// to ordinary string generation rather than emitting something certainly wrong.
//
// maxRepeat bounds unbounded quantifiers so that `.*` cannot produce a
// megabyte, and `{1,10000}` cannot either.
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

// alternation parses `a|b|c` and returns one branch. Every branch is parsed so
// the input is fully consumed, but only the chosen one contributes output.
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

// quantified parses one atom plus any trailing quantifier, and emits the atom
// the requested number of times. The atom is a generator rather than a string
// so that `[a-z]{4}` yields four different letters instead of one repeated.
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

// maybeLazy swallows a lazy marker. Greediness does not affect generation.
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
		// Not a quantifier after all — a literal brace. Rewind and let atom
		// handle it on the next pass.
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

// clampRepeat keeps generated strings sane. A spec saying {1,4096} is asking
// for a field bound, not for us to actually send 4096 characters.
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

	// Non-capturing groups are fine; other (?...) constructs are lookaround or
	// flags, which have no sensible generative reading.
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

// escapeSet handles a backslash inside a character class, where the result
// contributes to the enclosing set rather than generating directly.
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

// complement approximates a negated class over a printable ASCII alphabet.
// Generating from the full Unicode complement would be correct and useless.
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
