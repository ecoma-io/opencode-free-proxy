package caps

// Exhaustive checks over the production tables. The existing behavioral tests
// sample rows by hand; these run EVERY row so an added/removed/renamed entry
// cannot pass silently.

import (
	"strings"
	"testing"
)

// TestEveryExactRowResolvesToItself: every MODEL_CAPABILITIES key must resolve
// to exactly the row's flags through the exact-id path (baseModel == key, no
// vendor prefix, refine skipped — caps.go:79-84).
func TestEveryExactRowResolvesToItself(t *testing.T) {
	if len(modelCapabilities) == 0 {
		t.Fatal("modelCapabilities is empty — the exact table vanished")
	}
	for id, want := range modelCapabilities {
		id, want := id, want
		t.Run(id, func(t *testing.T) {
			if got := Resolve(id); got != want {
				t.Fatalf("Resolve(%q) = %+v, want exact row %+v", id, got, want)
			}
		})
	}
}

// TestEveryPatternRowMatchesItsOwnWitness: the table must contain no dead row
// and no reorder that shadows one. A witness is the row's own literal segments
// joined by "-" (pulled out of the COMPILED regexp, so it tracks the actual
// predicate, not the source pattern). Two properties:
//   - the witness must match the row itself ("not even itself matches" = the
//     pattern compiled to something degenerate);
//   - no EARLIER row may match it (a catch-all upserted too high would silently
//     shadow every later row below it).
//
// This is a necessary (not sufficient) condition for real-catalog
// reachability — a synthetic spelling can satisfy it while the catalog never
// produces one — but it is exactly the check that reports a surprise
// catch-all, a delete-shifted glob, or a pattern that no longer matches its
// own quote-meta'd segments.
func TestEveryPatternRowMatchesItsOwnWitness(t *testing.T) {
	if len(patternCapabilities) == 0 {
		t.Fatal("patternCapabilities is empty — the glob table vanished")
	}
	for i, row := range patternCapabilities {
		if row.re == nil {
			t.Fatalf("row %d: nil regexp (buildPatterns must always compile)", i)
		}
		w := witness(row.re.String())
		if !row.re.MatchString(w) {
			t.Fatalf("row %d: compiled regexp %q does not match its own witness %q", i, row.re.String(), w)
		}
		for j, prev := range patternCapabilities[:i] {
			if prev.re.MatchString(w) {
				t.Fatalf(
					"row %d (%q) is shadowed: earlier row %d (%q) already matches its witness %q",
					i, row.re.String(), j, prev.re.String(), w,
				)
			}
		}
	}
}

// witness strips the compilePattern wrapper — `(?i)^` + quote-meta'd segments
// joined by `.*` + `$` — and re-joins the literal segments with "-". Every
// segment is QuoteMeta'd, so the only bare `.*` in the source is the joiner:
// splitting on it then unescaping each segment yields a spelling that matches
// the whole regexp literally (the anchors absorb the joining "-" via `.*`).
func witness(re string) string {
	s := strings.TrimPrefix(re, "(?i)^")
	s = strings.TrimSuffix(s, "$")
	var b strings.Builder
	for i, seg := range strings.Split(s, ".*") {
		if i > 0 {
			b.WriteByte('-')
		}
		// regexp.QuoteMeta escapes a special char with a backslash: try-a-literal
		// `\.` in the source becomes `\.` — emit the escaped char verbatim.
		for j := 0; j < len(seg); j++ {
			if seg[j] == '\\' && j+1 < len(seg) {
				j++
			}
			b.WriteByte(seg[j])
		}
	}
	return b.String()
}
