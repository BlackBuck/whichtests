BIN := whichtests

.PHONY: build test lint demo replay mutate clean

build:
	go build -o $(BIN) ./cmd/whichtests
	go build -o $(BIN)-replay ./cmd/whichtests-replay
	go build -o $(BIN)-mutate ./cmd/whichtests-mutate

test:
	go test ./...

lint:
	go vet ./...
	gofmt -l . | grep -v '^example/' | (! grep .)

# Run the tool against its own example module.
demo: build
	./$(BIN) -C ./example -base HEAD -explain

# Replay this repo's own history. Point -repo elsewhere for a real measurement.
replay: build
	./$(BIN)-replay -repo . -module example -n 10

# Manufacture failures and check every one lands inside the selection.
mutate: build
	./$(BIN)-mutate -C ./example -n 6

clean:
	rm -f $(BIN) $(BIN)-replay $(BIN)-mutate
