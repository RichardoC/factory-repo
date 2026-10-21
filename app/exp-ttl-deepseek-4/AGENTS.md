# exp-ttl-deepseek-4

A CodeAF model-experiment app (issue #271): a standalone TTL + LRU cache with a
single-flight loader. It is a copy of the `app/exp-int-seed` fixture's rules,
not of its code — see README.md.

It is an experiment, not production code, and it must not be changed by
ordinary issues.

- Standard library only, no dependencies.
- In every module you touch: `gofmt -l .`, `go vet ./...`, `go build ./...`
  and `go test -race ./...`, all clean.
