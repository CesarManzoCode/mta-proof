.PHONY: check

# check: formato, vet y pruebas unitarias; no necesita red ni espacios de nombres. Lo corre el CI.
check:
	@if [ -f go.mod ]; then \
		unformatted="$$(gofmt -l .)"; \
		if [ -n "$$unformatted" ]; then echo "gofmt: $$unformatted"; exit 1; fi; \
		go vet ./... && go test -count=1 ./...; \
	else echo "check: no Go module yet"; fi
	@if [ -d verifier ]; then python3 -m unittest discover -s verifier -t . -p 'test_*.py'; \
	else echo "check: no verifier yet"; fi
