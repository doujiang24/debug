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

## go GC object ref flamegraph

compile:
```
cd cmd/viewcore && go build
```

prepare:
```
# 1. get a core file by using gcore
gcore $pid

# 2. get a copy of the the go binary file, eg, mosn.

# 3. prepare the FlameGraph tool and add it to PATH
git clone git@github.com:brendangregg/FlameGraph.git
#export PATH=/path-to-FlameGraph:$PATH
```

usage:
```
# 1. generate the ref file
./cmd/viewcore/viewcore ./mosn-core-file --exe ./mosn.binary objref ref.bt

# 2. generate svg file
stackcollapse-stap.pl ref.bt | flamegraph.pl > a.svg
```
