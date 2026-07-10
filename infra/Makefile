# Convenience wrapper around the AWS CLI for the Milestone 2 CloudFormation
# stacks. It contains no infrastructure logic of its own: every target shells
# out to `aws cloudformation`. CloudFormation remains the single source of
# truth. Requires the AWS CLI, configured credentials, and (for `validate`)
# cfn-lint.

PROJECT     ?= aiap
ENV         ?= dev
REGION      ?= us-east-1
CFN_DIR     := cloudformation

# spot (cheapest, the design default) or on-demand. Use on-demand if the
# account's Spot quota is zero — new accounts fail with "Max spot instance
# count exceeded" until a Spot quota increase is approved.
PURCHASE    ?= spot

# Named IAM roles require this capability acknowledgement.
CAPS        := CAPABILITY_NAMED_IAM
PARAMS      := ProjectName=$(PROJECT) Environment=$(ENV)
TAGS        := Project=$(PROJECT) Environment=$(ENV) ManagedBy=CloudFormation Milestone=2

# Per-stack extra parameter overrides. Only the compute stack declares
# PurchaseOption, so it must be passed to that stack alone — passing it to a
# stack that lacks it makes `aws cloudformation deploy` fail.
EXTRA_03-compute := PurchaseOption=$(PURCHASE)

# Stacks in dependency (deploy) order. Delete runs this list in reverse.
STACKS      := 01-network 02-iam 03-compute 04-storage 05-events 06-observability

.DEFAULT_GOAL := help
# Note: the per-stack `deploy-%` targets are deliberately NOT declared .PHONY.
# GNU Make skips implicit/pattern-rule search for phony targets, so listing
# `deploy-01-network` etc. here would silently stop the `deploy-%` recipe from
# ever running — `make deploy` would print "deployment complete" and do nothing.
.PHONY: help lint validate deploy delete outputs bootstrap install-hooks

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

bootstrap: ## One-time (admin): create the GitHub Actions OIDC provider + deploy role
	PROJECT=$(PROJECT) REGION=$(REGION) scripts/bootstrap-github-oidc.sh

install-hooks: ## Enable the repository's git hooks (one-time, per clone)
	@git -C .. config core.hooksPath .githooks
	@echo "git hooks enabled (core.hooksPath = .githooks)"

lint: ## Lint every template with cfn-lint
	cfn-lint $(CFN_DIR)/*.yaml

validate: ## Server-side validate every template (needs AWS credentials)
	@for s in $(STACKS); do \
		echo "validating $$s"; \
		aws cloudformation validate-template --region $(REGION) \
			--template-body file://$(CFN_DIR)/$$s.yaml >/dev/null || exit 1; \
	done
	@echo "all templates valid"

deploy: $(addprefix deploy-,$(STACKS)) ## Deploy all stacks in dependency order
	@echo "deployment complete"

# Deploy one stack. The stack name is prefixed so it is unambiguous in the
# console, e.g. aiap-dev-01-network. $(EXTRA_$*) adds any stack-specific
# parameter overrides (currently only compute's PurchaseOption).
deploy-%: ## Deploy a single stack, e.g. make deploy-01-network
	aws cloudformation deploy \
		--stack-name $(PROJECT)-$(ENV)-$* \
		--template-file $(CFN_DIR)/$*.yaml \
		--region $(REGION) --capabilities $(CAPS) \
		--parameter-overrides $(PARAMS) $(EXTRA_$*) \
		--tags $(TAGS)

delete: ## Delete all stacks in reverse order (retained buckets survive)
	@for s in $$(echo $(STACKS) | tr ' ' '\n' | tail -r 2>/dev/null || echo $(STACKS) | tr ' ' '\n' | tac); do \
		echo "deleting $(PROJECT)-$(ENV)-$$s"; \
		aws cloudformation delete-stack --region $(REGION) \
			--stack-name $(PROJECT)-$(ENV)-$$s; \
		aws cloudformation wait stack-delete-complete --region $(REGION) \
			--stack-name $(PROJECT)-$(ENV)-$$s; \
	done
	@echo "teardown complete (the artifact bucket is retained by design)"

outputs: ## Print the outputs of every deployed stack
	@for s in $(STACKS); do \
		echo "== $(PROJECT)-$(ENV)-$$s =="; \
		aws cloudformation describe-stacks --region $(REGION) \
			--stack-name $(PROJECT)-$(ENV)-$$s \
			--query 'Stacks[0].Outputs' --output table 2>/dev/null || echo "  (not deployed)"; \
	done
