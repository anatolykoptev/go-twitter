.RECIPEPREFIX = >

.PHONY: build test preflight

build:
> @GOWORK=off go build ./...

test:
> @GOWORK=off go test -count=1 ./...

preflight:
> @echo "==> gofmt -l ."
> @out=$$(GOWORK=off gofmt -l .); \
>   if [ -n "$$out" ]; then \
>     echo "FAIL: gofmt drift in the following files (run: gofmt -w <file>):"; \
>     echo "$$out"; \
>     exit 1; \
>   fi
> @echo "==> go vet ./..."
> @GOWORK=off go vet ./...
> @echo "==> go build ./..."
> @GOWORK=off go build ./...
> @echo "==> go test ./... -count=1"
> @GOWORK=off go test -count=1 ./...
