.PHONY: help bootstrap-secrets

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ {printf "  %-20s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

bootstrap-secrets: secrets/snmp_master ## Create secrets/snmp_master with a random key if missing.

secrets/snmp_master:
	@if [ ! -f secrets/snmp_master.example ]; then \
		echo "secrets/snmp_master.example not found — are you at the repo root?"; \
		exit 1; \
	fi
	@openssl rand -base64 32 > $@
	@chmod 644 $@
	@echo "Created $@ with a random 32-byte master key (mode 644)."
	@echo "Mode 644 is required so api/alert/snmp containers (non-root uid)"
	@echo "can read it via the docker-compose bind-mount. The file is"
	@echo "gitignored. Production resolves the master from Azure Key Vault."
	@echo ""
	@echo "If you have existing SNMP v3 credentials encrypted under a previous"
	@echo "master key, replace the contents of $@ with that key before starting."
