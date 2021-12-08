# Go Debug

[![Go Reference](https://pkg.go.dev/badge/golang.org/x/debug.svg)](https://pkg.go.dev/golang.org/x/debug)

This repository holds utilities and libraries for debugging Go programs.

**WARNING!**
Please expect breaking changes and unstable APIs.
Most of them are currently are at an early, *experimental* stage.

## Report Issues / Send Patches

This repository uses Gerrit for code changes. To learn how to submit changes to
this repository, see https://golang.org/doc/contribute.html.

The main issue tracker for the debug repository is located at
https://github.com/golang/go/issues. Prefix your issue with "x/debug:" in the
subject line, so it is easy to find.

## GC object ref flamegraph

usage:

```
# 1. generate the ref file
./cmd/viewcore/viewcore CORE-FILE --exe BINARY-FILE objref ref.bt

# 2. generate svg file
stackcollapse-stap.pl ref.bt | flamegraph.pl > a.svg
```

In the svg:
1. width means the bytes of a GC object and its child objects.
2. the path from bottom to top, means the reference path from a GC root object to a GC object.
