// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package oracle_test

// This file defines a test framework for oracle queries.
//
// The files beneath testdata/src/main contain Go programs containing
// query annotations of the form:
//
//   @verb id "select"
//
// where verb is the query mode (e.g. "callers"), id is a unique name
// for this query, and "select" is a regular expression matching the
// substring of the current line that is the query's input selection.
//
// The expected output for each query is provided in the accompanying
// .golden file.
//
// (Location information is not included because it's too fragile to
// display as text.  TODO(adonovan): think about how we can test its
// correctness, since it is critical information.)
//
// Run this test with:
// 	% go test code.google.com/p/go.tools/oracle -update
// to update the golden files.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"code.google.com/p/go.tools/oracle"
)

var updateFlag = flag.Bool("update", false, "Update the golden files.")

type query struct {
	id         string         // unique id
	verb       string         // query mode, e.g. "callees"
	posn       token.Position // position of of query
	filename   string
	start, end int // selection of file to pass to oracle
}

func parseRegexp(text string) (*regexp.Regexp, error) {
	pattern, err := strconv.Unquote(text)
	if err != nil {
		return nil, fmt.Errorf("can't unquote %s", text)
	}
	return regexp.Compile(pattern)
}

// parseQueries parses and returns the queries in the named file.
func parseQueries(t *testing.T, filename string) []*query {
	filedata, err := ioutil.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}

	// Parse the file once to discover the test queries.
	var fset token.FileSet
	f, err := parser.ParseFile(&fset, filename, filedata,
		parser.DeclarationErrors|parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}

	lines := bytes.Split(filedata, []byte("\n"))

	var queries []*query
	queriesById := make(map[string]*query)

	// Find all annotations of these forms:
	expectRe := regexp.MustCompile(`@([a-z]+)\s+(\S+)\s+(\".*)$`) // @verb id "regexp"
	for _, c := range f.Comments {
		text := strings.TrimSpace(c.Text())
		if text == "" || text[0] != '@' {
			continue
		}
		posn := fset.Position(c.Pos())

		// @verb id "regexp"
		match := expectRe.FindStringSubmatch(text)
		if match == nil {
			t.Errorf("%s: ill-formed query: %s", posn, text)
			continue
		}

		id := match[2]
		if prev, ok := queriesById[id]; ok {
			t.Errorf("%s: duplicate id %s", posn, id)
			t.Errorf("%s: previously used here", prev.posn)
			continue
		}

		selectRe, err := parseRegexp(match[3])
		if err != nil {
			t.Errorf("%s: %s", posn, err)
			continue
		}

		// Find text of the current line, sans query.
		// (Queries must be // not /**/ comments.)
		line := lines[posn.Line-1][:posn.Column-1]

		// Apply regexp to current line to find input selection.
		loc := selectRe.FindIndex(line)
		if loc == nil {
			t.Errorf("%s: selection pattern %s doesn't match line %q",
				posn, match[3], string(line))
			continue
		}

		// Assumes ASCII. TODO(adonovan): test on UTF-8.
		linestart := posn.Offset - (posn.Column - 1)

		// Compute the file offsets
		q := &query{
			id:       id,
			verb:     match[1],
			posn:     posn,
			filename: filename,
			start:    linestart + loc[0],
			end:      linestart + loc[1],
		}
		queries = append(queries, q)
		queriesById[id] = q
	}

	// Return the slice, not map, for deterministic iteration.
	return queries
}

// stripLocation removes a "file:line: " prefix.
func stripLocation(line string) string {
	if i := strings.Index(line, ": "); i >= 0 {
		line = line[i+2:]
	}
	return line
}

// doQuery poses query q to the oracle and writes its response and
// error (if any) to out.
func doQuery(out io.Writer, q *query, useJson bool) {
	fmt.Fprintf(out, "-------- @%s %s --------\n", q.verb, q.id)

	var buildContext = build.Default
	buildContext.GOPATH = "testdata"
	res, err := oracle.Query([]string{q.filename},
		q.verb,
		fmt.Sprintf("%s:#%d,#%d", q.filename, q.start, q.end),
		/*PTA-log=*/ nil, &buildContext)
	if err != nil {
		fmt.Fprintf(out, "\nError: %s\n", stripLocation(err.Error()))
		return
	}

	if useJson {
		// JSON output
		b, err := json.Marshal(res)
		if err != nil {
			fmt.Fprintf(out, "JSON error: %s\n", err.Error())
			return
		}
		var buf bytes.Buffer
		if err := json.Indent(&buf, b, "", "\t"); err != nil {
			fmt.Fprintf(out, "json.Indent failed: %s", err)
			return
		}
		out.Write(buf.Bytes())
	} else {
		// "plain" (compiler diagnostic format) output
		capture := new(bytes.Buffer) // capture standard output
		res.WriteTo(capture)
		for _, line := range strings.Split(capture.String(), "\n") {
			fmt.Fprintf(out, "%s\n", stripLocation(line))
		}
	}
}

func TestOracle(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		t.Skipf("skipping test on %q (no /usr/bin/diff)", runtime.GOOS)
	}

	for _, filename := range []string{
		"testdata/src/main/calls.go",
		"testdata/src/main/callgraph.go",
		"testdata/src/main/describe.go",
		"testdata/src/main/freevars.go",
		"testdata/src/main/implements.go",
		"testdata/src/main/imports.go",
		"testdata/src/main/peers.go",
		// JSON:
		"testdata/src/main/callgraph-json.go",
		"testdata/src/main/calls-json.go",
		"testdata/src/main/peers-json.go",
		"testdata/src/main/describe-json.go",
	} {
		useJson := strings.HasSuffix(filename, "-json.go")
		queries := parseQueries(t, filename)
		golden := filename + "lden"
		got := filename + "t"
		gotfh, err := os.Create(got)
		if err != nil {
			t.Errorf("Create(%s) failed: %s", got, err)
			continue
		}
		defer gotfh.Close()

		// Run the oracle on each query, redirecting its output
		// and error (if any) to the foo.got file.
		for _, q := range queries {
			doQuery(gotfh, q, useJson)
		}

		// Compare foo.got with foo.golden.
		cmd := exec.Command("/usr/bin/diff", "-u", golden, got) // assumes POSIX
		buf := new(bytes.Buffer)
		cmd.Stdout = buf
		if err := cmd.Run(); err != nil {
			t.Errorf("Oracle tests for %s failed: %s.\n%s\n",
				filename, err, buf)

			if *updateFlag {
				t.Logf("Updating %s...", golden)
				if err := exec.Command("/bin/cp", got, golden).Run(); err != nil {
					t.Errorf("Update failed: %s", err)
				}
			}
		}
	}
}
