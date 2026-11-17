SHELL := /bin/sh
SERVER := server
AGENT := agent

# Forwarded to agent/build/package.sh for release builds.
export MANIFEST_URL PUBLIC_KEY SIGNER_THUMBPRINT SIGNTOOL_CERT_SHA1 SIGN_COMMAND RELEASE_KEY NOTES_FILE MIN_VERSION MAKENSIS

.PHONY: help up down reset logs migrate migrate-down migrate-status seed api worker smoke smoke-multipart test fmt vet tidy build ci agent-test agent-e2e agent-cli agent-gui agent-launcher agent-release web-test web-build e2e-gallery migration-matrix loadtest payment-replay launch-drill

help:
	@echo "up             start postgres/redis/minio/gotenberg"
	@echo "down           stop the dev stack (keeps volumes)"
	@echo "reset          stop the dev stack and delete volumes (fresh DB)"
	@echo "logs           follow dev stack logs"
	@echo "migrate        apply embedded migrations"
	@echo "migrate-down   roll back the last migration"
	@echo "seed           create demo tenant + owner account"
	@echo "api            run the HTTP API on :8080"
	@echo "worker         run the async worker (ingest.verify, outbox, alert webhooks)"
	@echo "smoke          run the end-to-end smoke flow against a running API"
	@echo "smoke-multipart  smoke with a 40MiB multipart fixture"
	@echo "test           run unit tests"
	@echo "build          build api/worker/migrate/seed/smoke into server/bin"
	@echo "ci             isolated compose + migrate + seed + api/worker + smoke"
	@echo "agent-test     run the Windows agent unit tests"
	@echo "agent-e2e      headless import E2E (dual copy, dedupe, selftest)"
	@echo "agent-cli      build the headless agent CLI into agent/build/bin"
	@echo "agent-gui      build the Wails desktop app (requires wails + node)"
	@echo "agent-launcher build launcher.exe (current.txt -> version dir)"
	@echo "agent-release  build NSIS installer + signed manifest (VERSION=x.y.z CHANNEL=dev|stable)"
	@echo "web-test       typecheck + unit tests for admin/gallery and client miniapp typecheck"
	@echo "web-build      production build for admin/gallery and client H5/WeChat miniapp"
	@echo "e2e-gallery    isolated stack + Playwright selection, proof and delivery flows"
	@echo "migration-matrix  up -> down -> up plus the 0019/0018/0017/0015 data guards on a scratch DB"
	@echo "loadtest       load-test CLI (ARGS='-scenario booking-200 -concurrency 200')"
	@echo "payment-replay sandbox callback replay drill (ARGS='-scenario refund ...')"
	@echo "launch-drill   isolated stack: smoke fixture + payment/refund replay + loadtest"
	@echo "fmt vet tidy   Go housekeeping (server + agent)"

up:
	docker compose -f deploy/docker-compose.yml up -d

down:
	docker compose -f deploy/docker-compose.yml down

reset:
	docker compose -f deploy/docker-compose.yml down -v

logs:
	docker compose -f deploy/docker-compose.yml logs -f

migrate:
	cd $(SERVER) && go run ./cmd/migrate -dir up

migrate-down:
	cd $(SERVER) && go run ./cmd/migrate -dir down

migrate-status:
	cd $(SERVER) && go run ./cmd/migrate -dir status

seed:
	cd $(SERVER) && go run ./cmd/seed

api:
	cd $(SERVER) && go run ./cmd/api

worker:
	cd $(SERVER) && go run ./cmd/worker

smoke:
	cd $(SERVER) && go run ./cmd/smoke

smoke-multipart:
	cd $(SERVER) && go run ./cmd/smoke -size 41943040 -bulk 0

test:
	cd $(SERVER) && go test ./...
	cd $(AGENT) && CGO_ENABLED=0 go test ./...

build:
	cd $(SERVER) && go build -o bin/api ./cmd/api && go build -o bin/worker ./cmd/worker && go build -o bin/migrate ./cmd/migrate && go build -o bin/seed ./cmd/seed && go build -o bin/smoke ./cmd/smoke && go build -o bin/loadtest ./cmd/loadtest && go build -o bin/paymentreplay ./cmd/paymentreplay

agent-test:
	cd $(AGENT) && CGO_ENABLED=0 go test ./...

agent-e2e:
	bash scripts/agent-e2e.sh

agent-cli:
	cd $(AGENT) && CGO_ENABLED=0 go build -trimpath -o build/bin/pwf-agent ./cmd/pwf-agent

agent-gui:
	cd $(AGENT)/frontend && npm install && npm run build
	cd $(AGENT) && wails build -tags wails -s -skipbindings

agent-launcher:
	cd $(AGENT) && CGO_ENABLED=0 GOOS=windows go build -trimpath -ldflags "-s -w -H=windowsgui" -o build/bin/launcher.exe ./cmd/pwf-launcher && CGO_ENABLED=0 go build -trimpath -o build/bin/pwf-release.exe ./cmd/pwf-release

agent-release:
	@test -n "$(VERSION)" || (echo "usage: make agent-release VERSION=0.5.0 CHANNEL=dev [MANIFEST_URL=https://... PUBLIC_KEY=<base64> ...]" && exit 2)
	cd $(AGENT) && VERSION="$(VERSION)" CHANNEL="$(or $(CHANNEL),dev)" bash build/package.sh

ci:
	bash scripts/ci.sh

web-test:
	cd web-admin && npm ci && npm run typecheck && npm test
	cd web-gallery && npm ci && npm run typecheck && npm test
	cd web-client-miniapp && npm ci && npm run typecheck

web-build:
	cd web-admin && npm ci && npm run build
	cd web-gallery && npm ci && npm run build
	cd web-client-miniapp && npm ci && npm run build:h5 && npm run build:mp-weixin

e2e-gallery:
	bash scripts/gallery-e2e.sh

migration-matrix:
	bash scripts/migration-matrix.sh

loadtest:
	bash scripts/loadtest.sh $(ARGS)

payment-replay:
	bash scripts/payment-replay.sh $(ARGS)

launch-drill:
	bash scripts/launch-drill.sh $(ARGS)

fmt:
	cd $(SERVER) && gofmt -l -w .
	cd $(AGENT) && gofmt -l -w .

vet:
	cd $(SERVER) && go vet ./...
	cd $(AGENT) && go vet ./...

tidy:
	cd $(SERVER) && go mod tidy
	cd $(AGENT) && go mod tidy
