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

# Instance scheduler (07-scheduler): the instance to start/stop, its timezone,
# and where the packaged Go Lambdas live. INSTANCE_ID is required for scheduler
# targets; the code bucket defaults to the storage stack's artifact bucket.
INSTANCE_ID   ?=
TIMEZONE      ?= UTC
LAMBDA_DIR    := lambda
LAMBDA_PREFIX := scheduler

# Named IAM roles require this capability acknowledgement.
CAPS        := CAPABILITY_NAMED_IAM
PARAMS      := ProjectName=$(PROJECT) Environment=$(ENV)
TAGS        := Project=$(PROJECT) Environment=$(ENV) ManagedBy=CloudFormation Milestone=2

# Per-stack extra parameter overrides. Only the compute stack declares
# PurchaseOption, so it must be passed to that stack alone — passing it to a
# stack that lacks it makes `aws cloudformation deploy` fail.
EXTRA_03-compute := PurchaseOption=$(PURCHASE)

# Stack states in which the CREATE never succeeded. `aws cloudformation deploy`
# can only update an existing stack, so a stack stuck in one of these must be
# deleted before it can be recreated. Nothing durable is lost: the create did
# not complete, and the S3 bucket is retained independently of its stack.
WEDGED      := ROLLBACK_COMPLETE ROLLBACK_FAILED CREATE_FAILED DELETE_FAILED

# Stacks in dependency (deploy) order. Delete runs this list in reverse.
STACKS      := 01-network 02-iam 03-compute 04-storage 05-events 06-observability

.DEFAULT_GOAL := help
# Note: the per-stack `deploy-%` targets are deliberately NOT declared .PHONY.
# GNU Make skips implicit/pattern-rule search for phony targets, so listing
# `deploy-01-network` etc. here would silently stop the `deploy-%` recipe from
# ever running — `make deploy` would print "deployment complete" and do nothing.
.PHONY: lambda-test lambda-vet package deploy-scheduler delete-scheduler
.PHONY: help lint validate deploy delete outputs bootstrap install-hooks import-storage

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
#
# Self-healing: if the stack is wedged in a failed-create state (from an earlier
# aborted deploy), it is deleted first, since CloudFormation cannot update it.
# Healthy, absent, and in-progress stacks are left untouched.
deploy-%: ## Deploy a single stack, e.g. make deploy-01-network
	@stack=$(PROJECT)-$(ENV)-$*; \
	status=$$(aws cloudformation describe-stacks --stack-name $$stack \
		--region $(REGION) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	case " $(WEDGED) " in \
		*" $$status "*) \
			echo "$$stack is $$status — deleting before recreate"; \
			aws cloudformation delete-stack --stack-name $$stack --region $(REGION); \
			aws cloudformation wait stack-delete-complete --stack-name $$stack --region $(REGION); \
			;; \
	esac; \
	aws cloudformation deploy \
		--stack-name $$stack \
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

# Adopt an orphaned artifact bucket back into a fresh storage stack. Needed when
# 04-storage was deleted (by teardown or self-healing) while its bucket survived
# on DeletionPolicy: Retain — a plain `deploy-04-storage` then fails with
# "bucket ... already exists" because CloudFormation tries to CREATE it anew.
#
# CloudFormation import cannot create other resources in the same operation, so
# this runs in two steps: import the bucket alone (using a bucket-only template
# derived from 04-storage.yaml with the policy stripped), then a normal deploy of
# the full template to add the bucket policy back and reconcile any drift.
import-storage: ## Adopt an orphaned artifact bucket into a new 04-storage stack
	@stack=$(PROJECT)-$(ENV)-04-storage; \
	status=$$(aws cloudformation describe-stacks --stack-name $$stack \
		--region $(REGION) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	[ "$$status" = NONE ] || { echo "$$stack already exists ($$status) — use 'make deploy-04-storage', not import"; exit 1; }; \
	acct=$$(aws sts get-caller-identity --query Account --output text) || exit 1; \
	bucket=$(PROJECT)-$(ENV)-artifacts-$$acct-$(REGION); \
	aws s3api head-bucket --bucket $$bucket 2>/dev/null || { echo "bucket $$bucket does not exist — nothing to import; just 'make deploy-04-storage'"; exit 1; }; \
	echo "importing s3://$$bucket into $$stack"; \
	tmpl=$${TMPDIR:-/tmp}/aiap-04-storage-import.yaml; \
	rti=$${TMPDIR:-/tmp}/aiap-import-bucket.json; \
	awk '/^  ArtifactBucketPolicy:/{skip=1} /^Outputs:/{skip=0} !skip' $(CFN_DIR)/04-storage.yaml > $$tmpl; \
	printf '[{"ResourceType":"AWS::S3::Bucket","LogicalResourceId":"ArtifactBucket","ResourceIdentifier":{"BucketName":"%s"}}]' "$$bucket" > $$rti; \
	aws cloudformation create-change-set --region $(REGION) \
		--stack-name $$stack --change-set-name aiap-import-artifact-bucket \
		--change-set-type IMPORT \
		--resources-to-import file://$$rti \
		--template-body file://$$tmpl \
		--parameters ParameterKey=ProjectName,ParameterValue=$(PROJECT) ParameterKey=Environment,ParameterValue=$(ENV) \
		--capabilities $(CAPS); \
	aws cloudformation wait change-set-create-complete --region $(REGION) \
		--stack-name $$stack --change-set-name aiap-import-artifact-bucket; \
	aws cloudformation execute-change-set --region $(REGION) \
		--stack-name $$stack --change-set-name aiap-import-artifact-bucket; \
	aws cloudformation wait stack-import-complete --region $(REGION) --stack-name $$stack; \
	echo "bucket imported — reconciling full template (adds the bucket policy)"
	@$(MAKE) deploy-04-storage

outputs: ## Print the outputs of every deployed stack
	@for s in $(STACKS); do \
		echo "== $(PROJECT)-$(ENV)-$$s =="; \
		aws cloudformation describe-stacks --region $(REGION) \
			--stack-name $(PROJECT)-$(ENV)-$$s \
			--query 'Stacks[0].Outputs' --output table 2>/dev/null || echo "  (not deployed)"; \
	done

# --- Instance scheduler (optional add-on; requires the Go toolchain) --------

lambda-vet: ## Vet the scheduler Lambda module
	cd $(LAMBDA_DIR) && go vet ./...

lambda-test: ## Test the scheduler Lambda module
	cd $(LAMBDA_DIR) && go test ./...

# Resolve the artifact bucket from the storage stack, so nothing is hard-coded.
define artifact_bucket
$(shell aws cloudformation describe-stacks --stack-name $(PROJECT)-$(ENV)-04-storage \
	--region $(REGION) --query "Stacks[0].Outputs[?OutputKey=='ArtifactBucketName'].OutputValue" \
	--output text 2>/dev/null)
endef

package: ## Build both Go Lambdas (arm64) and upload their zips to the artifact bucket
	@bucket="$(strip $(call artifact_bucket))"; \
	[ -n "$$bucket" ] || { echo "artifact bucket not found — deploy 04-storage first"; exit 1; }; \
	echo "packaging to s3://$$bucket/$(LAMBDA_PREFIX)/"; \
	for h in start stop; do \
		echo "  building $$h"; \
		( cd $(LAMBDA_DIR) && GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o /tmp/aiap-bootstrap ./cmd/$$h ); \
		( cd /tmp && cp aiap-bootstrap bootstrap && zip -q $$h.zip bootstrap && rm -f bootstrap ); \
		aws s3 cp /tmp/$$h.zip s3://$$bucket/$(LAMBDA_PREFIX)/$$h.zip --region $(REGION); \
	done; \
	echo "packaged"

deploy-scheduler: ## Deploy the instance scheduler (needs INSTANCE_ID=i-...; run `package` first)
	@[ -n "$(INSTANCE_ID)" ] || { echo "set INSTANCE_ID=i-... (the instance to schedule)"; exit 1; }
	@bucket="$(strip $(call artifact_bucket))"; \
	[ -n "$$bucket" ] || { echo "artifact bucket not found — deploy 04-storage first"; exit 1; }; \
	aws cloudformation deploy \
		--stack-name $(PROJECT)-$(ENV)-07-scheduler \
		--template-file $(CFN_DIR)/07-scheduler.yaml \
		--region $(REGION) --capabilities $(CAPS) \
		--parameter-overrides $(PARAMS) InstanceId=$(INSTANCE_ID) ScheduleTimezone=$(TIMEZONE) \
			LambdaCodeBucket=$$bucket StartCodeKey=$(LAMBDA_PREFIX)/start.zip StopCodeKey=$(LAMBDA_PREFIX)/stop.zip \
		--tags $(TAGS)

delete-scheduler: ## Delete the instance scheduler stack
	aws cloudformation delete-stack --region $(REGION) --stack-name $(PROJECT)-$(ENV)-07-scheduler
	aws cloudformation wait stack-delete-complete --region $(REGION) --stack-name $(PROJECT)-$(ENV)-07-scheduler
