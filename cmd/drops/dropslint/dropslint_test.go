package dropslint_test

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/bernardoforcillo/drops/cmd/drops/dropslint"
)

// TestClean is the test that matters. The clean package is a page of
// ordinary drops code, correctly written; it carries no want comments,
// so any diagnostic from any rule fails the test. A rule that starts
// crying wolf fails here first.
func TestClean(t *testing.T) {
	for _, a := range dropslint.All() {
		t.Run(a.Name, func(t *testing.T) {
			analysistest.Run(t, analysistest.TestData(), a, "dropslint.corpus/clean")
		})
	}
}

func TestUnfilteredWrite(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), dropslint.UnfilteredWrite, "dropslint.corpus/writes")
}

func TestUnboundedRead(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), dropslint.UnboundedRead, "dropslint.corpus/reads")
}

func TestLoopLoad(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), dropslint.LoopLoad, "dropslint.corpus/loops")
}
