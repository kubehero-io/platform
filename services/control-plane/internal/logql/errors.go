// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import "fmt"

// ErrorKind classifies evaluation failures so API layers can answer
// with the right status (400 vs 429/422).
type ErrorKind int

const (
	// KindInvalid: the query cannot be evaluated as written (bad grid,
	// pipeline errors, ambiguous matching). The caller must change it.
	KindInvalid ErrorKind = iota
	// KindLimit: the query is valid but too expensive (too many series,
	// lines or cells). Narrowing or aggregating it helps.
	KindLimit
)

// Error is a classified evaluation error. Parse errors are *ParseError.
type Error struct {
	Kind ErrorKind
	Msg  string
}

func (e *Error) Error() string { return "logql: " + e.Msg }

// Invalidf builds a KindInvalid error.
func Invalidf(format string, args ...any) error {
	return &Error{Kind: KindInvalid, Msg: fmt.Sprintf(format, args...)}
}

// Limitf builds a KindLimit error.
func Limitf(format string, args ...any) error {
	return &Error{Kind: KindLimit, Msg: fmt.Sprintf(format, args...)}
}
