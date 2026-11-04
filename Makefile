# sw-test-runner — build, test, and wiki helpers.
# Recipes use real tabs (GNU make). Run `make help` for targets.

BIN    := bin
CMDS   := sw-test-runner seaweed-cloud swblock weedblock sweeds3 testops-dashboard testops-controller
GOFLAGS ?=

.PHONY: all help build test vet clean wiki wiki-build $(addprefix build-,$(CMDS))

all: build

help:
	@echo "targets:"
	@echo "  build         build every cmd/ binary into ./$(BIN)"
	@echo "  build-<name>  build one binary ($(CMDS))"
	@echo "  test          go test ./... -count=1"
	@echo "  vet           go vet ./..."
	@echo "  wiki          serve the MkDocs wiki (needs requirements-docs.txt)"
	@echo "  wiki-build    build the static wiki site into ./site"
	@echo "  clean         remove ./$(BIN) and ./site"
	@echo "Seaweed existing-host pack: ./$(BIN)/seaweed-cloud direct --inventory INV --config CFG --out NEW_DIR"

build: $(addprefix build-,$(CMDS))

# Phony names need explicit rules: implicit pattern search skips phony targets.
$(addprefix build-,$(CMDS)): build-%:
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -o $(BIN)/$* ./cmd/$*

# Retain ad-hoc builds of other existing cmd/ entries.
build-%:
	@mkdir -p $(BIN)
	go build $(GOFLAGS) -o $(BIN)/$* ./cmd/$*

test:
	go test ./... -count=1

vet:
	go vet ./...

wiki:
	mkdocs serve

wiki-build:
	mkdocs build

clean:
	rm -rf $(BIN) site
