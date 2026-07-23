.PHONY: test e2e e2e-tls e2e-up e2e-test e2e-down

lint-fix:
	go run github.com/google/addlicense -c "JC-Lab" -l "EPL-2.0" -s .

# Unit / race test suite.
test:
	go test -race ./...

# Full e2e cycle: build images, bring the stack up, run scenarios, tear down.
e2e: e2e-up e2e-test e2e-down

# Same cycle with TLS between the components. The plaintext run above is the
# default; this one exists because a TLS-only defect (a missing SAN, an address
# advertised without a scheme) is invisible to it.
e2e-tls:
	E2E_TLS=1 $(MAKE) e2e-up e2e-test e2e-down

# Build images and bring the integration stack up (idempotent).
e2e-up:
	./scripts/e2e-up.sh

# Run the e2e scenarios against an already-running stack.
e2e-test:
	./e2e/run.sh

# Tear the stack down and remove volumes + generated artifacts.
e2e-down:
	./scripts/e2e-down.sh
