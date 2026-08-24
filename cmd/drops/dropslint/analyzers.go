package dropslint

import "golang.org/x/tools/go/analysis"

// All returns the rules in the order they are worth reading: the one
// that loses data, the one that stalls the server, the one that makes
// it slow.
func All() []*analysis.Analyzer {
	return []*analysis.Analyzer{UnfilteredWrite, UnboundedRead, LoopLoad}
}
