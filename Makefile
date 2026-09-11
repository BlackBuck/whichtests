BIN := whichtests

.PHONY: build test lint demo clean

build:
	go build -o $(BIN) ./cmd/whichtests

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l . | grep -v '^example/' | (! grep .)

# Run the tool against its own example module.
demo: build
	./$(BIN) -C ./example -base HEAD -explain

clean:
	rm -f $(BIN)
