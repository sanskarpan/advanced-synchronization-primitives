.PHONY: build test race bench clean lint examples coverage

BINARY := syncprimitives-server
COVERAGE_THRESHOLD := 70

build:
	go build -o $(BINARY) ./cmd/server

test:
	go test -timeout 120s ./...

race:
	go test -race -count=3 -timeout 120s ./...

bench:
	go test -bench=. -benchmem -timeout 120s ./internal/primitives/

examples:
	go build ./examples/...

lint:
	go vet ./...

clean:
	rm -f $(BINARY)
	go clean -testcache

coverage:
	# Run tests only for packages that have testable (non-main) code.
	go test -coverprofile=coverage.out -covermode=atomic -timeout 120s \
	    ./internal/... ./web/...
	@total=$$(go tool cover -func=coverage.out | grep '^total:' | awk '{print $$3}' | tr -d '%'); \
	echo "Total coverage: $${total}%"; \
	awk -v t="$${total}" -v th="$(COVERAGE_THRESHOLD)" \
	    'BEGIN { if (t+0 < th+0) { print "FAIL: coverage " t "% < " th "%"; exit 1 } \
	             else { print "OK: coverage " t "% >= " th "%" } }'

verify:
	go mod verify
