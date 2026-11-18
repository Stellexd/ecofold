.PHONY: all build build-daemon build-gui build-linux run-daemon run-gui clean test

BINARY_DAEMON=bin/ecofold-daemon
BINARY_GUI=bin/ecofold-gui

all: build

build: build-daemon build-gui

build-daemon:
	@echo "🔨 Compilando ecofold-daemon..."
	go build -o $(BINARY_DAEMON) ./cmd/ecofold-daemon

# La GUI usa Fyne (Go puro, sin frontend ni Node.js). Su driver gráfico
# necesita cgo: un compilador de C en macOS (Xcode Command Line Tools) y, en
# Linux, gcc y las cabeceras de X11/OpenGL (en Arch: libx11 libxcursor
# libxrandr libxinerama libxi libxxf86vm libglvnd).
build-gui:
	@echo "🔨 Compilando ecofold-gui..."
	CGO_ENABLED=1 go build -o $(BINARY_GUI) ./cmd/ecofold-gui

build-linux:
	@echo "🔨 Cross-compilando para Linux (solo verificación de compilación)..."
	@echo "⚠️  CGO_ENABLED=0: se compila internal/gpu/stub_linux.go, SIN control de TDP."
	@echo "   ecofold-gui se compila sin interfaz gráfica (Fyne necesita cgo)."
	@echo "   Para los binarios reales, compila en Linux con CGO_ENABLED=1 (make build)."
	CGO_ENABLED=0 GOOS=linux go build ./...

run-daemon: build-daemon
	@if [ "$$(uname)" = "Linux" ]; then \
		echo "🚀 Corriendo daemon en Linux (requiere sudo para NVML)..."; \
		sudo ./$(BINARY_DAEMON); \
	else \
		echo "🍎 Corriendo daemon en macOS (Modo Simulador/Mock)..."; \
		./$(BINARY_DAEMON); \
	fi

# La GUI corre como el usuario normal, nunca con sudo. Habla con el daemon por
# su socket; si el daemon no está, lo indica y reintenta sola.
run-gui: build-gui
	./$(BINARY_GUI)

test:
	go test -race -v ./...

clean:
	rm -rf bin/
