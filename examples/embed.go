// Package examples embeds the canonical example so standalone binaries and
// containers emit the same question bank as the checked-in configuration.
package examples

import _ "embed"

//go:embed scoring.json
var scoring string

func Scoring() string { return scoring }
