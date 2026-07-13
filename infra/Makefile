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

# Instance type. Empty uses the template's default (t3.xlarge). Override it when
# the default type has no Spot capacity in this environment's AZ — a launch then
# fails with "There is no Spot capacity available that matches your request",
# which is a capacity problem, not a price or quota one. Check before choosing:
#
#   aws ec2 get-spot-placement-scores --instance-types c5.xlarge \
#     --target-capacity 1 --target-capacity-unit-type units \
#     --region-names us-east-1 --single-availability-zone
#
# A single instance in a single AZ has no capacity flexibility by construction;
# diversifying across types and AZs needs an Auto Scaling group (Milestone 19).
INSTANCE_TYPE ?=

# Instance scheduler (07-scheduler): the instance to start/stop, its timezone,
# and where the packaged Go Lambdas live. INSTANCE_ID is required for scheduler
# targets; the code bucket defaults to the storage stack's artifact bucket.
INSTANCE_ID   ?=
TIMEZONE      ?= UTC
LAMBDA_DIR    := lambda

# The Go Lambdas, grouped by the stack that deploys them and the S3 key prefix
# their zips are uploaded under. Both groups are built by `package`.
SCHED_FUNCS   := start stop
SCHED_PREFIX  := scheduler
SPOT_FUNCS    := interruption statechange
SPOT_PREFIX   := spot

# Named IAM roles require this capability acknowledgement.
CAPS        := CAPABILITY_NAMED_IAM
PARAMS      := ProjectName=$(PROJECT) Environment=$(ENV)
TAGS        := Project=$(PROJECT) Environment=$(ENV) ManagedBy=CloudFormation Milestone=2
# Spot interruption handling is Milestone 3, so its resources are tagged as such.
SPOT_TAGS   := Project=$(PROJECT) Environment=$(ENV) ManagedBy=CloudFormation Milestone=3

# Per-stack extra parameter overrides. Only the compute stack declares
# PurchaseOption and InstanceType, so they must be passed to that stack alone —
# passing a parameter to a stack that lacks it makes `aws cloudformation deploy`
# fail. InstanceType is omitted entirely when unset, so the template's default
# stands rather than being overridden with an empty string.
EXTRA_03-compute := PurchaseOption=$(PURCHASE) $(if $(INSTANCE_TYPE),InstanceType=$(INSTANCE_TYPE))

# Stack states in which the CREATE never succeeded. `aws cloudformation deploy`
# can only update an existing stack, so a stack stuck in one of these must be
# deleted before it can be recreated. Nothing durable is lost: the create did
# not complete, and the S3 bucket is retained independently of its stack.
# REVIEW_IN_PROGRESS is a stack that only ever had a change set created (e.g. a
# never-executed import) and has no real resources — also cleared by deleting.
WEDGED      := ROLLBACK_COMPLETE ROLLBACK_FAILED CREATE_FAILED DELETE_FAILED REVIEW_IN_PROGRESS

# Stacks in dependency (deploy) order. Delete runs this list in reverse.
STACKS      := 01-network 02-iam 03-compute 04-storage 05-events 06-observability

.DEFAULT_GOAL := help
# Note: the per-stack `deploy-%` targets are deliberately NOT declared .PHONY.
# GNU Make skips implicit/pattern-rule search for phony targets, so listing
# `deploy-01-network` etc. here would silently stop the `deploy-%` recipe from
# ever running — `make deploy` would print "deployment complete" and do nothing.
.PHONY: lambda-test lambda-vet package deploy-scheduler delete-scheduler
.PHONY: spot deploy-spot delete-spot simulate-interruption
.PHONY: help lint validate deploy delete outputs bootstrap install-hooks import-storage

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

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
# Self-healing, in two passes before the deploy:
#  1. A stack wedged in a failed-create state (from an earlier aborted deploy)
#     is deleted, since CloudFormation cannot update it. Healthy, absent, and
#     in-progress stacks are left untouched.
#  2. For 04-storage only: if the stack is absent but its artifact bucket still
#     exists (retained on DeletionPolicy: Retain after a delete/teardown), the
#     bucket is imported into a fresh stack first — otherwise the deploy would
#     try to CREATE a bucket whose deterministic name already exists and fail
#     with "bucket already exists". Import can't create other resources in the
#     same step, so it adopts the bucket alone; the deploy that follows adds the
#     bucket policy back and reconciles drift.
deploy-%: ## Deploy a single stack, e.g. make deploy-01-network
	@stack=$(PROJECT)-$(ENV)-$*; \
	status=$$(aws cloudformation describe-stacks --stack-name $$stack \
		--region $(REGION) --query 'Stacks[0].StackStatus' --output text 2>/dev/null || echo NONE); \
	case " $(WEDGED) " in \
		*" $$status "*) \
			echo "$$stack is $$status — deleting before recreate"; \
			aws cloudformation delete-stack --stack-name $$stack --region $(REGION); \
			aws cloudformation wait stack-delete-complete --stack-name $$stack --region $(REGION); \
			status=NONE; \
			;; \
	esac; \
	if [ "$*" = "04-storage" ] && [ "$$status" = NONE ]; then \
		acct=$$(aws sts get-caller-identity --query Account --output text 2>/dev/null); \
		bucket=$(PROJECT)-$(ENV)-artifacts-$$acct-$(REGION); \
		if [ -n "$$acct" ] && aws s3api head-bucket --bucket "$$bucket" 2>/dev/null; then \
			echo "$$bucket is orphaned (no stack) — importing before deploy"; \
			tmpl=$${TMPDIR:-/tmp}/aiap-04-storage-import.yaml; \
			rti=$${TMPDIR:-/tmp}/aiap-import-bucket.json; \
			awk '/^  ArtifactBucketPolicy:/{skip=1} /^Outputs:/{skip=0} !skip' $(CFN_DIR)/04-storage.yaml > $$tmpl; \
			printf '[{"ResourceType":"AWS::S3::Bucket","LogicalResourceId":"ArtifactBucket","ResourceIdentifier":{"BucketName":"%s"}}]' "$$bucket" > $$rti; \
			aws cloudformation create-change-set --region $(REGION) \
				--stack-name $$stack --change-set-name aiap-import-artifact-bucket \
				--change-set-type IMPORT --resources-to-import file://$$rti \
				--template-body file://$$tmpl \
				--parameters ParameterKey=ProjectName,ParameterValue=$(PROJECT) ParameterKey=Environment,ParameterValue=$(ENV) \
				--capabilities $(CAPS); \
			aws cloudformation wait change-set-create-complete --region $(REGION) \
				--stack-name $$stack --change-set-name aiap-import-artifact-bucket; \
			aws cloudformation execute-change-set --region $(REGION) \
				--stack-name $$stack --change-set-name aiap-import-artifact-bucket; \
			aws cloudformation wait stack-import-complete --region $(REGION) --stack-name $$stack; \
		fi; \
	fi; \
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

# Fix the storage stack alone when its artifact bucket was orphaned (retained on
# DeletionPolicy: Retain after a delete/teardown, so a fresh deploy hits "bucket
# already exists"). This is just `deploy-04-storage`, which now detects the
# orphaned bucket and imports it automatically — so `make deploy` already handles
# this with no separate step. Kept as a named shortcut for that one stack.
import-storage: ## Fix the storage stack alone (adopts an orphaned artifact bucket)
	@$(MAKE) deploy-04-storage

# The add-on stacks are listed too. They are not in STACKS (they are not part of
# `make deploy`), but a stack you deployed is a stack you want the outputs of.
# Undeployed ones simply print "(not deployed)".
outputs: ## Print the outputs of every deployed stack
	@for s in $(STACKS) 07-scheduler 08-spot; do \
		echo "== $(PROJECT)-$(ENV)-$$s =="; \
		aws cloudformation describe-stacks --region $(REGION) \
			--stack-name $(PROJECT)-$(ENV)-$$s \
			--query 'Stacks[0].Outputs' --output table 2>/dev/null || echo "  (not deployed)"; \
	done

# --- Go Lambdas (optional add-ons; require the Go toolchain) ----------------
#
# The core `deploy` above needs nothing but the AWS CLI. The scheduler (07) and
# the Spot interruption handlers (08) are deployed separately because they need
# Go to build and an artifact bucket to upload to.

lambda-vet: ## Vet the Lambda module
	cd $(LAMBDA_DIR) && go vet ./...

lambda-test: ## Test the Lambda module
	cd $(LAMBDA_DIR) && go test ./...

# Resolve the artifact bucket from the storage stack, so nothing is hard-coded.
define artifact_bucket
$(shell aws cloudformation describe-stacks --stack-name $(PROJECT)-$(ENV)-04-storage \
	--region $(REGION) --query "Stacks[0].Outputs[?OutputKey=='ArtifactBucketName'].OutputValue" \
	--output text 2>/dev/null)
endef

package: ## Build all four Go Lambdas (arm64) and upload their zips to the artifact bucket
	@bucket="$(strip $(call artifact_bucket))"; \
	[ -n "$$bucket" ] || { echo "artifact bucket not found — deploy 04-storage first"; exit 1; }; \
	build_upload() { \
		prefix="$$1"; fn="$$2"; \
		echo "  building $$fn"; \
		( cd $(LAMBDA_DIR) && GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o /tmp/aiap-bootstrap ./cmd/$$fn ); \
		( cd /tmp && cp aiap-bootstrap bootstrap && zip -q $$fn.zip bootstrap && rm -f bootstrap ); \
		aws s3 cp /tmp/$$fn.zip s3://$$bucket/$$prefix/$$fn.zip --region $(REGION); \
	}; \
	echo "packaging to s3://$$bucket/"; \
	for f in $(SCHED_FUNCS); do build_upload $(SCHED_PREFIX) $$f || exit 1; done; \
	for f in $(SPOT_FUNCS);  do build_upload $(SPOT_PREFIX)  $$f || exit 1; done; \
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
			LambdaCodeBucket=$$bucket StartCodeKey=$(SCHED_PREFIX)/start.zip StopCodeKey=$(SCHED_PREFIX)/stop.zip \
		--tags $(TAGS)

delete-scheduler: ## Delete the instance scheduler stack
	aws cloudformation delete-stack --region $(REGION) --stack-name $(PROJECT)-$(ENV)-07-scheduler
	aws cloudformation wait stack-delete-complete --region $(REGION) --stack-name $(PROJECT)-$(ENV)-07-scheduler

# --- Spot interruption handling (Milestone 3) -------------------------------

spot: package deploy-spot ## Build the handlers and deploy Spot interruption handling

deploy-spot: ## Deploy Spot interruption handling (run `package` first, or use `make spot`)
	@bucket="$(strip $(call artifact_bucket))"; \
	[ -n "$$bucket" ] || { echo "artifact bucket not found — deploy 04-storage first"; exit 1; }; \
	aws cloudformation deploy \
		--stack-name $(PROJECT)-$(ENV)-08-spot \
		--template-file $(CFN_DIR)/08-spot.yaml \
		--region $(REGION) --capabilities $(CAPS) \
		--parameter-overrides $(PARAMS) LambdaCodeBucket=$$bucket \
			InterruptionCodeKey=$(SPOT_PREFIX)/interruption.zip \
			StateChangeCodeKey=$(SPOT_PREFIX)/statechange.zip \
		--tags $(SPOT_TAGS)

delete-spot: ## Delete the Spot interruption handling stack
	aws cloudformation delete-stack --region $(REGION) --stack-name $(PROJECT)-$(ENV)-08-spot
	aws cloudformation wait stack-delete-complete --region $(REGION) --stack-name $(PROJECT)-$(ENV)-08-spot

# Rehearse an interruption without waiting for AWS to take the instance away.
#
# This invokes the handler with the exact payload EventBridge would deliver, so
# it exercises the handler, its IAM policy, the metric, and the platform event —
# everything except EventBridge's own delivery. It cannot make EC2 emit a real
# aws.ec2 event: AWS reserves the "aws." source prefix, so PutEvents will not
# accept one. A true end-to-end interruption needs AWS Fault Injection Service
# (see SPOT.md), or patience.
simulate-interruption: ## Invoke the interruption handler with a sample event (INSTANCE_ID=i-...)
	@[ -n "$(INSTANCE_ID)" ] || { echo "set INSTANCE_ID=i-... (the instance to simulate an interruption for)"; exit 1; }
	@printf '{"version":"0","detail-type":"EC2 Spot Instance Interruption Warning","source":"aws.ec2","time":"%s","detail":{"instance-id":"%s","instance-action":"terminate"}}' \
		"$$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$(INSTANCE_ID)" > /tmp/aiap-interruption.json
	@aws lambda invoke --region $(REGION) \
		--function-name $(PROJECT)-$(ENV)-spot-interruption \
		--payload fileb:///tmp/aiap-interruption.json /dev/stdout
