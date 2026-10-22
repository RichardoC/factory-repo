# Running the harness tests

The module is its own Go module (`codeaf-harness`); run everything from
`.github/codeaf/`:

```
go test -race ./...
```

## Running a selection, and why the issue's regex does not work

Issue #223 was written with this `## End-to-end check`:

```
go -C .github/codeaf test -race -run "TestContract\|TestCountReports\|TestFetchPRFiles" .
```

It matches no test. `go test -run` takes a Go **RE2** regular expression, and
RE2 has no `\|` alternation operator: the backslash is not an escape there, so
`\|` is a *literal pipe*, and the pattern reads "the text `TestContract` or
`TestCountReports` or `TestFetchPRFiles` with a `|` right after it". No test
name contains a pipe, so nothing is selected — and `go test` exits **0** when
it selects nothing:

```
$ go test -race -run "TestContract\|TestCountReports\|TestFetchPRFiles" .
ok  	codeaf-harness	1.319s [no tests to run]     # exit status 0: vacuous
```

The runnable, non-vacuous equivalent is the same command with a real
alternation — `|`, unescaped:

```
go -C .github/codeaf test -race -run 'TestContract|TestCountReports|TestFetchPRFiles' .
```

Inside double quotes that would be `"TestContract|TestCountReports|TestFetchPRFiles"`;
the single quotes above are the shell's, and keep the pattern out of the
shell's own way. This selects six tests:

```
--- PASS: TestContract
--- PASS: TestCountReports
--- PASS: TestCountReportsAcrossPages
--- PASS: TestCountReportsSinglePage
--- PASS: TestFetchPRFilesPaginates
--- PASS: TestFetchPRFilesFailsClosed
```

## Proving a selection is not vacuous

A `-run` that matches nothing still exits 0, so a green end-to-end check proves
nothing on its own. Assert the selection reached a test: count the `--- PASS`
lines under `-v`, and fail when there are none.

```
go -C .github/codeaf test -race -v -run 'TestContract|TestCountReports|TestFetchPRFiles' . |
  grep -c '^--- PASS'          # 6, or any count > 0
```

The broken pattern under the same assertion prints **0** and the run is
revealed as vacuous:

```
go -C .github/codeaf test -race -v -run 'TestContract\|TestCountReports\|TestFetchPRFiles' . |
  grep -c '^--- PASS'          # 0: matched no test
```
