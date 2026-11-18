package indexer

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anjanikumarjangid/codeweft/internal/store"
)

// junitTest matches the JUnit 4 and 5 annotations that mark a test method.
var junitTest = regexp.MustCompile(`@(Test|ParameterizedTest|RepeatedTest|TestFactory|TestTemplate)\b`)

// isTestEntry reports whether a function in a test file is a test the test
// runner calls, as opposed to a helper:
//
//   - Go: top-level TestXxx, BenchmarkXxx, FuzzXxx and ExampleXxx functions
//     (the name continues with an upper-case letter, a digit, "_" or nothing);
//   - Java: methods annotated @Test (or another JUnit test annotation), and
//     JUnit 3 style public testXxx methods;
//   - Python: test_xxx functions and methods, as pytest and unittest find them.
func isTestEntry(lang string, s store.Symbol) bool {
	switch lang {
	case "go":
		if s.Kind != "func" {
			return false
		}
		for _, prefix := range []string{"Test", "Benchmark", "Fuzz", "Example"} {
			if rest, ok := strings.CutPrefix(s.Name, prefix); ok {
				r, _ := utf8.DecodeRuneInString(rest)
				return rest == "" || r == '_' || unicode.IsUpper(r) || unicode.IsDigit(r)
			}
		}
		return false
	case "java":
		return junitTest.MatchString(s.Signature) ||
			(strings.HasPrefix(s.Name, "test") && strings.Contains(s.Signature, "public "))
	case "python":
		return strings.HasPrefix(s.Name, "test")
	}
	return false
}
