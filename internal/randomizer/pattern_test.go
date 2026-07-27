package randomizer

import (
	"math/rand"
	"regexp"
	"testing"
)

// The strongest possible assertion for a pattern generator: compile the same
// regex the API server will use, and require every generated sample to match.
func TestGeneratePatternMatchesItsOwnRegex(t *testing.T) {
	patterns := []string{
		`^[0-9]{15}$`,                    // microchip id
		`^[A-Z]{2}-\d{4}$`,               // ticket reference
		`^(cat|dog|ferret)$`,             // alternation
		`^(?:red|green|blue)-\d{2}$`,     // non-capturing group
		`[a-z]+`,                         // unbounded quantifier
		`^\d{3}-\d{2}-\d{4}$`,            // segmented number
		`^[a-zA-Z0-9_]{3,12}$`,           // username
		`^SKU-[A-Z0-9]{8}$`,              // literal prefix
		`^[a-f0-9]{8}$`,                  // hex
		`^v\d+\.\d+\.\d+$`,               // semver-ish
		`^colou?r$`,                      // optional character
		`^\w{4,6}$`,                      // word class
		`^[^0-9]{5}$`,                    // negated class
		`^(a|b)(c|d)\d{2}$`,              // adjacent groups
		`^[A-Z]{1,3}[0-9]{1,4}[A-Z]{1}$`, // mixed bounds
	}

	rng := rand.New(rand.NewSource(1))

	for _, pattern := range patterns {
		t.Run(pattern, func(t *testing.T) {
			re, err := regexp.Compile(pattern)
			if err != nil {
				t.Fatalf("test pattern does not compile: %v", err)
			}

			for i := 0; i < 200; i++ {
				got, ok := GeneratePattern(rng, pattern)
				if !ok {
					t.Fatalf("GeneratePattern(%q) reported unsupported", pattern)
				}
				if !re.MatchString(got) {
					t.Fatalf("generated %q does not match %q", got, pattern)
				}
			}
		})
	}
}

// Unsupported constructs must report failure rather than emit something that
// will certainly be rejected. Falling back to ordinary string generation is a
// better outcome than confidently sending garbage.
func TestGeneratePatternRejectsUnsupported(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	unsupported := []string{
		`(?=foo)bar`, // lookahead
		`(?!foo)bar`, // negative lookahead
		`(a)\1`,      // backreference
		`[z-a]`,      // inverted range
		`(unclosed`,  // unbalanced
		`[unclosed`,  // unbalanced class
		``,           // empty
		`*abc`,       // quantifier with no atom
	}

	for _, pattern := range unsupported {
		t.Run(pattern, func(t *testing.T) {
			if got, ok := GeneratePattern(rng, pattern); ok {
				t.Errorf("GeneratePattern(%q) = %q, ok=true; want unsupported", pattern, got)
			}
		})
	}
}

func TestGeneratePatternRespectsRepetitionBounds(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	for i := 0; i < 100; i++ {
		got, ok := GeneratePattern(rng, `^[a-z]{3,5}$`)
		if !ok {
			t.Fatal("unexpected unsupported")
		}
		if len(got) < 3 || len(got) > 5 {
			t.Fatalf("length %d out of bounds for {3,5}: %q", len(got), got)
		}
	}
}

// An unbounded quantifier must not produce an unbounded string; a daemon
// sending megabyte fields for a week is a different kind of outage.
func TestGeneratePatternCapsUnboundedQuantifiers(t *testing.T) {
	rng := rand.New(rand.NewSource(3))

	for i := 0; i < 100; i++ {
		got, ok := GeneratePattern(rng, `^.*$`)
		if !ok {
			t.Fatal("unexpected unsupported")
		}
		if len(got) > maxRepeat {
			t.Fatalf("`.*` produced %d chars, want at most %d", len(got), maxRepeat)
		}
	}
}

func TestGeneratePatternVariesRepeatedAtoms(t *testing.T) {
	rng := rand.New(rand.NewSource(11))

	// [a-z]{8} repeated 8 times should not be the same letter eight times;
	// that would mean the atom generator is evaluated once and reused.
	var sawVariation bool
	for i := 0; i < 50 && !sawVariation; i++ {
		got, ok := GeneratePattern(rng, `^[a-z]{8}$`)
		if !ok {
			t.Fatal("unexpected unsupported")
		}
		for _, r := range got {
			if r != rune(got[0]) {
				sawVariation = true
				break
			}
		}
	}
	if !sawVariation {
		t.Error("every generated string was a single repeated character")
	}
}

func TestGeneratePatternIsDeterministicForASeed(t *testing.T) {
	const pattern = `^[A-Z]{3}-\d{5}$`

	first, _ := GeneratePattern(rand.New(rand.NewSource(42)), pattern)
	second, _ := GeneratePattern(rand.New(rand.NewSource(42)), pattern)

	if first != second {
		t.Errorf("same seed produced %q then %q", first, second)
	}
}
