.PHONY: all build build-daemon build-gui build-linux run-daemon clean test

BINARY_DAEMON=bin/ecofold-daemon
BINARY_GUI=bin/ecofold-gui

all: build

build: build-daemon build-gui

build-daemon:
	@echo "🔨 Compilando ecofold-daemon..."
	go build -o $(BINARY_DAEMON) ./cmd/ecofold-daemon

build-gui:
	@echo "🔨 Compilando ecofold-gui..."
	go build -o $(BINARY_GUI) ./cmd/ecofold-gui

build-linux:
	@echo "🔨 Cross-compilando para Linux (solo verificación de compilación)..."
	@echo "⚠️  CGO_ENABLED=0: se compila internal/gpu/stub_linux.go, SIN control de TDP."
	@echo "   Para el binario real, compila en Linux con CGO_ENABLED=1 (make build)."
	CGO_ENABLED=0 GOOS=linux go build ./...

run-daemon: build-daemon
	@if [ "$$(uname)" = "Linux" ]; then \
		echo "🚀 Corriendo daemon en Linux (requiere sudo para NVML)..."; \
		sudo ./$(BINARY_DAEMON); \
	else \
		echo "🍎 Corriendo daemon en macOS (Modo Simulador/Mock)..."; \
		./$(BINARY_DAEMON); \
	fi

test:
	go test -race -v ./...

clean:
	rm -rf bin/
