VERSION ?= dev
IMAGE_PREFIX ?= ghcr.io/tucunar
NERDCTL ?= nerdctl
CONTAINERD_ADDRESS ?= /run/k0s/containerd.sock
CONTAINERD_NAMESPACE ?= k8s.io
PLATFORM ?= linux/amd64
REPO_ROOT ?= $(abspath ../..)
CONTAINERFILE ?= $(REPO_ROOT)/tools/tucunar-cell/Containerfile
BUILD_CONTEXT ?= $(REPO_ROOT)

SERVICES := swift canary-evaluator condor hummingbird \
	kingfisher kingbird pigeon woodpecker heron eagle kestrel hawk \
	raven phoenix lark owl toucan shiitake

.PHONY: test vet build images image-% check-containerd

test:
	go test ./...

vet:
	go vet ./...

build:
	go build ./...

images:
	@set -e; for service in $(SERVICES); do \
		$(NERDCTL) --address $(CONTAINERD_ADDRESS) --namespace $(CONTAINERD_NAMESPACE) build \
			--file $(CONTAINERFILE) --platform $(PLATFORM) --build-arg SERVICE=$$service --build-arg VERSION=$(VERSION) \
			-t $(IMAGE_PREFIX)/$$service:$(VERSION) $(BUILD_CONTEXT); \
	done

image-%:
	@case " $(SERVICES) " in *" $* "*) ;; *) echo "unknown service: $*" >&2; exit 2;; esac
	$(NERDCTL) --address $(CONTAINERD_ADDRESS) --namespace $(CONTAINERD_NAMESPACE) build \
		--file $(CONTAINERFILE) --platform $(PLATFORM) --build-arg SERVICE=$* --build-arg VERSION=$(VERSION) \
		-t $(IMAGE_PREFIX)/$*:$(VERSION) $(BUILD_CONTEXT)

check-containerd:
	@command -v $(NERDCTL) >/dev/null || { echo "missing $(NERDCTL); install nerdctl with BuildKit support" >&2; exit 1; }
	@test -S $(CONTAINERD_ADDRESS) || { echo "missing containerd socket: $(CONTAINERD_ADDRESS)" >&2; exit 1; }
	@$(NERDCTL) --address $(CONTAINERD_ADDRESS) --namespace $(CONTAINERD_NAMESPACE) info >/dev/null
