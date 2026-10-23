.PHONY: build check verify

# build: binarios de cmd/ en bin/. Los mutantes (bin/mut/) llegan con k3.
build:
	@mkdir -p bin
	go build -o bin/ ./cmd/...

# check: formato, vet y pruebas unitarias; no necesita red ni espacios de nombres. Lo corre el CI.
check:
	@if [ -f go.mod ]; then \
		unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt: $$unformatted"; exit 1; fi; \
		go vet ./... && go test -count=1 ./...; \
	else echo "check: no Go module yet"; fi
	@if [ -d verifier ]; then python3 -m unittest discover -s verifier -t . -p 'test_*.py'; \
	else echo "check: no verifier yet"; fi

# verify: el comando único. Corre dentro de un scope de systemd de usuario (memory.peak
# cuenta toda la corrida, D17) y de un espacio de nombres de usuario, red y montaje
# con solo lo (2.3, D06); el verificador levanta lo. Sale con 0 solo si todo pasa.
verify: build
	systemd-run --user --scope --quiet -- unshare --user --map-root-user --net --mount python3 -m verifier verify --out out
