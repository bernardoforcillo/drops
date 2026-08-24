// The corpus is a module so analysistest loads it the way the go
// command would, imports and all. It is replaced onto the checkout
// above it so the fixtures compile against the drops in this tree —
// the same reason cmd/drops itself carries a replace.
module dropslint.corpus

go 1.25.0

require github.com/bernardoforcillo/drops v0.6.0

replace github.com/bernardoforcillo/drops => ../../../..
