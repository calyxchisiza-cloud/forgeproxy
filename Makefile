.PHONY: test race demo serve

test:
	go test ./...

race:
	go test -race -count=1 ./...

demo:
	go run ./cmd/demo

serve:
	go run ./cmd/forgeproxy
