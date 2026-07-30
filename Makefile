.PHONY: build run clean test web web-install vet

BINARY=notion-manager

build: web
	go build -o $(BINARY) ./cmd/notion-manager/

build-go:
	go build -o $(BINARY) ./cmd/notion-manager/

run:
	go run ./cmd/notion-manager/

web-install:
	cd web && npm install

web:
	cd web && npm run build
	rm -rf internal/web/dist
	cp -r web/dist internal/web/dist

clean:
	rm -f $(BINARY)
	rm -rf web/dist
	rm -rf internal/web/dist

test:
	go test ./...

vet:
	go vet ./...
