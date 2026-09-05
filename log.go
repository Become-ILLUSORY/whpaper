package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

type logger struct {
	verbose bool
	out     io.Writer
}

func newLogger(verbose bool) *logger {
	return &logger{verbose: verbose, out: os.Stderr}
}

func (l *logger) Debugf(format string, args ...any) {
	if l == nil || !l.verbose {
		return
	}
	fmt.Fprintf(l.out, "· "+format+"\n", args...)
}

func (l *logger) Infof(format string, args ...any) {
	if l == nil {
		return
	}
	fmt.Fprintf(l.out, format+"\n", args...)
}

func (l *logger) Warnf(format string, args ...any) {
	if l == nil {
		return
	}
	fmt.Fprintf(l.out, "! "+format+"\n", args...)
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
