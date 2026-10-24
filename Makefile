.PHONY: deps build check verify

# Mutantes del contrato 4.3: una etiqueta de compilación cada uno.
MUTANTS := mut_no_backoff mut_retry_permanent mut_ignore_conn_limit mut_ack_before_persist mut_shared_queue mut_exceed_warmup

# deps: lo único que usa la red, una vez: módulos Go y Haraka fijado por su
# package-lock.json (D08). npm ci no ejecuta scripts de instalación no aprobados.
deps:
	go mod download
	cd external/haraka && npm ci

# build: binarios de cmd/ en bin/ y, por mutante, los mismos con su etiqueta en
# bin/mut/<nombre>/, para que el verificador corra la suite con --bin bin/mut/<nombre>.
build:
	@mkdir -p bin
	go build -o bin/ ./cmd/...
	@for m in $(MUTANTS); do \
		mkdir -p bin/mut/$$m && go build -tags $$m -o bin/mut/$$m/ ./cmd/... || exit 1; \
	done

# check: formato, vet y pruebas unitarias; no necesita red ni espacios de nombres. Lo corre el CI.
check:
	@if [ -f go.mod ]; then \
		unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt: $$unformatted"; exit 1; fi; \
		go vet ./... && go test -count=1 ./... || exit 1; \
		for m in $(MUTANTS); do \
			go vet -tags $$m ./internal/engine && \
			go test -count=1 -tags $$m -run '^TestMutant' ./internal/engine || exit 1; \
		done; \
	else echo "check: no Go module yet"; fi
	@if [ -d verifier ]; then python3 -m unittest discover -s verifier -t . -p 'test_*.py'; \
	else echo "check: no verifier yet"; fi

# verify: el comando único. Corre dentro de un scope de systemd de usuario (memory.peak
# cuenta toda la corrida, D17) y de un espacio de nombres de usuario, red y montaje
# con solo lo (2.3, D06); el verificador levanta lo. Sale con 0 solo si todo pasa.
verify: build
	systemd-run --user --scope --quiet -- unshare --user --map-root-user --net --mount python3 -m verifier verify --out out
