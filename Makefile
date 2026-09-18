.PHONY: check

check:   ## the gate: runs whatever test suite exists (language decided in the first design bead)
	@if [ -f go.mod ]; then go test ./...; \
	elif [ -f package.json ]; then npm test; \
	elif [ -f pyproject.toml ]; then python3 -m pytest -q; \
	else echo "check: no test suite yet (design phase)"; fi
