IMG ?= vaultic/k8s-operator:dev

.PHONY: all
all: fmt vet test build

.PHONY: fmt
fmt:
	gofmt -l . | tee /dev/stderr | (! read -r)

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test -v ./...

.PHONY: build
build:
	CGO_ENABLED=0 GOOS=linux go build -o bin/manager ./cmd

.PHONY: run
run:
	go run ./cmd

CONTROLLER_GEN ?= go run sigs.k8s.io/controller-gen/cmd/controller-gen@v0.16.4

.PHONY: manifests
manifests: ## Regenerate CRD YAML and RBAC role.yaml from kubebuilder markers.
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd
	$(CONTROLLER_GEN) rbac:roleName=manager-role paths=./... output:rbac:artifacts:config=config/rbac

.PHONY: generate
generate: ## Regenerate zz_generated.deepcopy.go.
	$(CONTROLLER_GEN) object:headerFile="" paths=./api/...

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

.PHONY: install
install:
	kubectl apply -f config/crd/

.PHONY: uninstall
uninstall:
	kubectl delete -f config/crd/ --ignore-not-found

# config/manager/deployment.yaml's checked-in `image:` is the production default (a GHCR release
# tag) — substitute $(IMG) at apply time so `make deploy` still defaults to the local :dev image
# (IMG's default above) and stays overridable (`IMG=ghcr.io/vaultic-dev/vaultic-k8s-operator:vX.Y.Z
# make deploy`) without needing Kustomize/Helm just for this one field.
.PHONY: deploy
deploy:
	kubectl apply -f config/manager/namespace.yaml
	kubectl apply -f config/rbac/
	sed 's#^\( *image: \).*#\1$(IMG)#' config/manager/deployment.yaml | kubectl apply -f -

.PHONY: undeploy
undeploy:
	kubectl delete -f config/manager/deployment.yaml --ignore-not-found
	kubectl delete -f config/rbac/ --ignore-not-found
	kubectl delete -f config/manager/namespace.yaml --ignore-not-found
