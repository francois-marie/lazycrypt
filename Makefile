.PHONY: build test lint clean install

BINARY := lazycrypt

build:
	go build -o $(BINARY) .

test:
	go test -v -race -count=1 ./...

coverage:
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out

lint:
	go vet ./...

clean:
	rm -f $(BINARY) coverage.out

install:
	go install .
