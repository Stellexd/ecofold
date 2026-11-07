.PHONY: all build build-daemon build-gui run-daemon clean test

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

run-daemon: build-daemon
	@if [ "$$(uname)" = "Linux" ]; then \n		echo "🚀 Corriendo daemon en Linux (requiere sudo para NVML)..."; \n		sudo ./$(BINARY_DAEMON); \n	else \n		echo "🍎 Corriendo daemon en macOS (Modo Simulador/Mock)..."; \n		./$(BINARY_DAEMON); \n	fi

test:
	go test -v ./...

clean:
	rm -rf bin/
